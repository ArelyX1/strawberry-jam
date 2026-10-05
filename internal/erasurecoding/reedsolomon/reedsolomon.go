package reedsolomon

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"unsafe"

	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/rustlib"
)

// Las firmas de la libreria de Rust se declaran con tipos de Go y no con cgo,
// porque todas las llamadas pasan por purego y cgo solo aportaba aqui el ancho
// de size_t. Sin el, el mismo fichero compila para cualquier destino con
// CGO_ENABLED=0, que es lo que permite construir sin un compilador de C
// cruzado.
//
// El coste de quitarlo es que el ancho de size_t queda fijado a 8 bytes. En un
// destino de 32 bits el array siguiente tendria longitud negativa, que es un
// error de compilacion aqui y no un fallo silencioso mas adelante.
var _ [unsafe.Sizeof(uintptr(0)) - 8]struct{}

const (
	MaxShards    = 65535 // Original + Recovery shards should equal this, a limitation of the reed-solomon-simd library.
	MaxShardSize = 1024  // The graypayer calls for a shard size of 2 so this is a decent sized maximum if this changes.
)

var (
	// Note: All slice parameters use uintptr because purego on ARM64 doesn't support slices
	reedSolomonEncode func(
		originalShardsCount uint64,
		recoveryShardsCount uint64,
		shardSize uint64,
		originalShards uintptr,
		originalShardsLen uint64,
		recoveryShardsOut uintptr,
		recoveryShardsLen uint64,
	) (cerr int)

	reedSolomonDecode func(
		originalShardsCount uint64,
		recoveryShardsCount uint64,
		shardSize uint64,
		originalShards uintptr,
		originalShardsLen uint64,
		originalShardsIndexes uintptr,
		recoveryShards uintptr,
		recoveryShardsLen uint64,
		recoveryShardsIndexes uintptr,
		recoveredShards uintptr,
		recoveredShardsLength uint64,
		recoveredShardsIndexesOut uintptr,
	) (cerr int)
)

type Encoder struct {
	originalShardsCount int
	recoveryShardsCount int
}

// Create a new reed solomon encoder with the given original shards count and
// recovery shards count.
func New(originalShardsCount, recoveryShardsCount int) (*Encoder, error) {
	if recoveryShardsCount > math.MaxInt-originalShardsCount {
		return nil, fmt.Errorf("shard count overflow")
	}

	if originalShardsCount+recoveryShardsCount > MaxShards {
		return nil, fmt.Errorf("too many total shards")
	}

	return &Encoder{
		originalShardsCount: originalShardsCount,
		recoveryShardsCount: recoveryShardsCount,
	}, nil
}

// Takes a slice of data to encode and chunks the data into shards. The
// resulting shards contain the original shards and enough space for the
// recovery shards. Shards can be passed directly to encode. Shards will have a
// length of the original shards count + the recovery shards count. Data is not
// copied, so the input data should not be modified after.
func (r *Encoder) Chunk(data []byte) ([][]byte, error) {
	if len(data) > constants.MaxWorkPackageSize {
		return nil, errors.New("data length too long")
	}
	// Need at least two bytes per chunk.
	if len(data) < r.originalShardsCount*2 {
		return nil, errors.New("data length too short")
	}
	shardSize := (len(data) + r.originalShardsCount - 1) / r.originalShardsCount
	shards := make([][]byte, r.originalShardsCount+r.recoveryShardsCount)

	// Handle all full-sized chunks.
	for i := 0; i < r.originalShardsCount-1; i++ {
		start := i * shardSize
		shards[i] = data[start : start+shardSize]
	}

	// Handle last chunk with padding.
	start := (r.originalShardsCount - 1) * shardSize
	padded := make([]byte, shardSize)
	copy(padded, data[start:])
	shards[r.originalShardsCount-1] = padded

	return shards, nil
}

// Takes shards and fills in the recovery shards. The first original shard count
// indexes should contain the original shards, the remaining recovery shard
// indexes are then filled with recovery shards.
// This can be used to implement C ∶ ⟦B2⟧342 → ⟦B2⟧1023 in the graypaper. (Apendix H, v0.5.2-4)
func (r *Encoder) Encode(
	shards [][]byte) error {
	if ShardCount(shards[:r.originalShardsCount]) != r.originalShardsCount {
		return errors.New("too few original shards")
	}

	if len(shards) != (r.originalShardsCount + r.recoveryShardsCount) {
		return errors.New("not enough space for recovery shards")
	}

	shardSize := ShardSize(shards)
	if shardSize == 0 || shardSize > MaxShardSize {
		return errors.New("invalid shard size")
	}

	// A code with no recovery shards has no parity to compute: the shards that come
	// out are the shards that went in. The reed-solomon library rejects this shape,
	// but a chain spec that asks for no redundancy is a spec that can still be
	// encoded, and the answer a caller needs is an encoding rather than an error.
	if r.recoveryShardsCount == 0 {
		return nil
	}

	flatOriginalShards := make([]byte, r.originalShardsCount*shardSize)
	for i, s := range shards[:r.originalShardsCount] {
		if len(s) != shardSize {
			return errors.New("inconsistent shard size")
		}
		copy(flatOriginalShards[i*shardSize:], s)
	}

	recoveryShardsOut := make([]byte, r.recoveryShardsCount*shardSize)

	result := reedSolomonEncode(
		uint64(r.originalShardsCount),
		uint64(r.recoveryShardsCount),
		uint64(shardSize),
		slicePtr(flatOriginalShards),
		uint64(len(flatOriginalShards)),
		slicePtr(recoveryShardsOut),
		uint64(len(recoveryShardsOut)),
	)

	// Keep slices alive until after the FFI call completes
	runtime.KeepAlive(flatOriginalShards)
	runtime.KeepAlive(recoveryShardsOut)

	if result != 0 {
		return errors.New("unable to encode data")
	}

	for i := 0; i < r.recoveryShardsCount; i++ {
		start := i * shardSize
		shards[i+r.originalShardsCount] = recoveryShardsOut[start : start+shardSize]
	}
	return nil
}

// Takes a slice of shards which should have at least original shard count
// shards and fills in the missing original shards. Missing shards are with nil
// or a length of zero before decoding.
// This can be used to implement R ∶ ℘⟨⎧⎩B2, N1023⎫⎭⟩342 → ⟦B2⟧342 in the graypaper. (Apendix H, v0.5.2-4)
func (r *Encoder) Decode(shards [][]byte) error {
	if ShardCount(shards) < r.originalShardsCount {
		return errors.New("too few shards")
	}
	shardSize := ShardSize(shards)
	if shardSize == 0 || shardSize > MaxShardSize {
		return errors.New("invalid shard size")
	}

	// With no recovery shards there is nothing to recover from, so the only thing
	// left to do is refuse when a shard is actually missing. The data of a spec
	// without parity is only whole while every shard of it is.
	if r.recoveryShardsCount == 0 {
		if ShardCount(shards[:r.originalShardsCount]) != r.originalShardsCount {
			return errors.New("no recovery shards to recover a missing shard from")
		}
		return nil
	}

	flatOriginalShards := []byte{}
	flatOriginalShardsIndexes := []uint64{}
	for i, s := range shards[:r.originalShardsCount] {
		if len(s) != 0 {
			if len(s) != shardSize {
				return errors.New("inconsistent shard size")
			}
			flatOriginalShards = append(flatOriginalShards, s...)
			flatOriginalShardsIndexes = append(flatOriginalShardsIndexes, uint64(i))
		}
	}

	flatRecoveryShards := []byte{}
	flatRecoveryShardsIndexes := []uint64{}
	for i, s := range shards[r.originalShardsCount:] {
		if len(s) != 0 {
			if len(s) != shardSize {
				return errors.New("inconsistent shard size")
			}
			flatRecoveryShards = append(flatRecoveryShards, s...)
			flatRecoveryShardsIndexes = append(flatRecoveryShardsIndexes, uint64(i))
		}
	}

	shardCountOriginal := ShardCount(shards[:r.originalShardsCount])
	// Shards we already have aren't restored.
	restoredShardsCount := r.originalShardsCount - shardCountOriginal
	restoredShards := make([]byte, restoredShardsCount*shardSize)
	restoredShardsIndexes := make([]uint64, restoredShardsCount)

	result := reedSolomonDecode(
		uint64(r.originalShardsCount),
		uint64(r.recoveryShardsCount),
		uint64(shardSize),
		slicePtr(flatOriginalShards),
		uint64(len(flatOriginalShards)),
		slicePtrSizeT(flatOriginalShardsIndexes),
		slicePtr(flatRecoveryShards),
		uint64(len(flatRecoveryShards)),
		slicePtrSizeT(flatRecoveryShardsIndexes),
		slicePtr(restoredShards),
		uint64(len(restoredShards)),
		slicePtrSizeT(restoredShardsIndexes))

	// Keep slices alive until after the FFI call completes
	runtime.KeepAlive(flatOriginalShards)
	runtime.KeepAlive(flatOriginalShardsIndexes)
	runtime.KeepAlive(flatRecoveryShards)
	runtime.KeepAlive(flatRecoveryShardsIndexes)
	runtime.KeepAlive(restoredShards)
	runtime.KeepAlive(restoredShardsIndexes)

	if result != 0 {
		return errors.New("unable to decode data")
	}

	for i := 0; i < restoredShardsCount; i++ {
		start := i * shardSize
		index := int(restoredShardsIndexes[i])
		shards[index] = restoredShards[start : start+shardSize]
	}

	return nil
}

// Returns the first non nil or length zero shard length.
func ShardSize(shards [][]byte) int {
	for _, shard := range shards {
		if len(shard) != 0 {
			return len(shard)
		}
	}
	return 0
}

// Returns the count of non nil or length zero shards.
func ShardCount(shards [][]byte) int {
	count := 0
	for _, shard := range shards {
		if len(shard) != 0 {
			count++
		}
	}
	return count
}

// slicePtr returns a pointer to the first element of a byte slice.
// For empty slices, returns a dummy non-null pointer (our rust implementation requires non-null).
func slicePtr(s []byte) uintptr {
	if len(s) == 0 {
		return uintptr(unsafe.Pointer(&struct{}{}))
	}
	return uintptr(unsafe.Pointer(&s[0]))
}

// slicePtrSizeT returns a pointer to the first element of a uint64 slice.
// For empty slices, returns a dummy non-null pointer (our rust implementation requires non-null).
func slicePtrSizeT(s []uint64) uintptr {
	if len(s) == 0 {
		return uintptr(unsafe.Pointer(&struct{}{}))
	}
	return uintptr(unsafe.Pointer(&s[0]))
}

func init() {
	// Load the Rust shared library in the init function.
	libPath, err := getErasurecodingLibaryPath()
	if err != nil {
		fmt.Println("Failed to load erasure coding library path:", err)
		os.Exit(1)
	}

	// Load the Rust shared library.
	lib, err := rustlib.Open(libPath)
	if err != nil {
		fmt.Println("Failed to load erasure coding library:", err)
		os.Exit(1)
	}

	// Register the Rust FFI functions with Go using purego.
	lib.Bind(&reedSolomonEncode, "reed_solomon_encode")
	lib.Bind(&reedSolomonDecode, "reed_solomon_decode")
}

func getErasurecodingLibaryPath() (string, error) {
	return rustlib.WriteOnce("strawberry-erasurecoding-lib", rustLibraryName, rustLibraryBytes)
}
