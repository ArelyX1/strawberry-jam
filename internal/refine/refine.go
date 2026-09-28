package refine

import (
	"errors"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/pvm"
	host_call_pkg "github.com/eigerco/strawberry/internal/pvm/host_call"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state"
	"github.com/eigerco/strawberry/internal/work"
	"github.com/eigerco/strawberry/pkg/log"
	"github.com/eigerco/strawberry/pkg/serialization/codec/jam"
)

const (
	RefineCost = 10
)

var (
	ErrBad = errors.New("service’s code was not available for lookup") // BAD
	ErrBig = errors.New("code beyond the maximum size allowed")        // BIG
)

type RefinePVMInvoker interface {
	InvokePVM(
		itemIndex uint32,
		workPackage work.Package,
		authorizerHashOutput []byte,
		importedSegments []work.Segment,
		exportOffset uint64,
	) ([]byte, []work.Segment, uint64, error)
}

type Refine struct {
	state state.State
}

func New(state state.State) *Refine {
	return &Refine{state: state}
}

// InvokePVM ΨR(N,P,B, ⟦⟦G⟧⟧, N) → (B ∪ E, ⟦G⟧, N_G)
func (r *Refine) InvokePVM(
	itemIndex uint32, // i
	workPackage work.Package, // p
	authorizerHashOutput []byte, // o
	importedSegments []work.Segment, // i
	exportOffset uint64, // ς
) ([]byte, []work.Segment, uint64, error) {
	// w = p_w[i]
	w := workPackage.WorkItems[itemIndex]

	packageBytes, err := jam.Marshal(workPackage)
	if err != nil {
		return nil, nil, 0, err
	}

	// let a = E(ws, wy, H(p), px, pu)
	args, err := jam.Marshal(struct {
		ServiceIndex      block.ServiceId
		WorkPayload       []byte
		WorkPackageHash   crypto.Hash
		RefinementContext block.RefinementContext
		AuthorizerHash    crypto.Hash
	}{
		ServiceIndex:      w.ServiceId,
		WorkPayload:       w.Payload,
		WorkPackageHash:   crypto.HashData(packageBytes),
		RefinementContext: workPackage.Context,
		AuthorizerHash:    workPackage.AuthCodeHash,
	})

	// if s ∉ K(δ) ∨ Λ(δ[s], ct, c) = ∅ then (BAD, [])
	s, ok := r.state.Services[w.ServiceId]
	if !ok {
		return nil, nil, 0, ErrBad
	}

	// E(↕m, c) = Λ(δ[ws], (px)t, wc)
	encodedCodeWithMeta := s.LookupPreimage(w.ServiceId, workPackage.Context.LookupAnchor.Timeslot, w.CodeHash)
	if encodedCodeWithMeta == nil {
		return nil, nil, 0, ErrBad
	}

	var pvmCode service.CodeWithMetadata
	err = jam.Unmarshal(encodedCodeWithMeta, &pvmCode)
	if err != nil {
		return nil, nil, 0, err
	}

	// if |Λ(δ[s], ct, c)| > W_C then (BIG, [])
	if len(pvmCode.Code) > constants.MaxSizeServiceCode {
		return nil, nil, 0, ErrBig
	}

	// F ∈ Ω⟨(D⟨N → M⟩, ⟦G⟧)⟩∶ (n, ϱ, φ, μ, (m, e))
	//
	// The guest's entry point and ecall indices are read out of the blob: the
	// linker assigns both, so they cannot be assumed here.
	guest, guestErr := pvm.PrepareGuest(pvmCode.Code, "")
	if guestErr != nil {
		return nil, nil, 0, ErrBad
	}
	guestCode, guestErr := guest.Code()
	if guestErr != nil {
		return nil, nil, 0, ErrBad
	}

	hostCall := func(ecall uint64, gasCounter pvm.Gas, regs pvm.Registers, mem pvm.Memory, ctxPair pvm.RefineContextPair) (pvm.Gas, pvm.Registers, pvm.Memory, pvm.RefineContextPair, error) {
		hostCall, ok := guest.HostCallID(ecall)
		if !ok {
			return gasCounter, regs, mem, ctxPair, pvm.ErrPanicf("unknown ecall %d", ecall)
		}

		log.VM.Debug().
			Str("host_call", host_call_pkg.HostCallName(hostCall)).
			Uint32("service_id", uint32(w.ServiceId)).
			Str("phase", "refine").
			Msg("Host call invoked")

		switch hostCall {
		case host_call_pkg.GasID:
			gasCounter, regs, err = host_call_pkg.GasRemaining(gasCounter, regs)
		case host_call_pkg.FetchID:
			zeroHash := crypto.Hash{}
			// TODO we need to pass the preimage data `x` instead of nil (where x = [[x ∣ (H(x), ∣x∣) <− wx] ∣ w <− pw])
			gasCounter, regs, mem, err = host_call_pkg.Fetch(gasCounter, regs, mem, &workPackage, &zeroHash, authorizerHashOutput, &itemIndex, importedSegments, nil, nil)
		case host_call_pkg.HistoricalLookupID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.HistoricalLookup(gasCounter, regs, mem, ctxPair, w.ServiceId, r.state.Services, workPackage.Context.LookupAnchor.Timeslot)
		case host_call_pkg.ExportID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.Export(gasCounter, regs, mem, ctxPair, exportOffset)
		case host_call_pkg.MachineID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.Machine(gasCounter, regs, mem, ctxPair)
		case host_call_pkg.PeekID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.Peek(gasCounter, regs, mem, ctxPair)
		case host_call_pkg.PokeID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.Poke(gasCounter, regs, mem, ctxPair)
		case host_call_pkg.PagesID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.Pages(gasCounter, regs, mem, ctxPair)
		case host_call_pkg.InvokeID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.Invoke(gasCounter, regs, mem, ctxPair)
		case host_call_pkg.ExpungeID:
			gasCounter, regs, mem, ctxPair, err = host_call_pkg.Expunge(gasCounter, regs, mem, ctxPair)
		default:
			regs[pvm.R7] = uint64(host_call_pkg.WHAT)
			gasCounter -= RefineCost
		}
		// otherwise if ϱ′ < 0
		if gasCounter < 0 {
			return gasCounter, regs, mem, ctxPair, pvm.ErrOutOfGas
		}
		return gasCounter, regs, mem, ctxPair, err
	}

	// (g, r, (m, e)) = ΨM(Λ(δ[w_s], (p_x)t, w_c), 0, w_g, a, F, (∅, []))∶
	remainingGas, result, ctxPair, err := pvm.InvokeWholeProgram(guestCode, guest.Entry, pvm.UGas(w.GasLimitRefine), args, hostCall, pvm.RefineContextPair{
		IntegratedPVMMap: make(map[uint64]pvm.IntegratedPVM),
		Segments:         []work.Segment{},
	})

	// if r ∈ {∞, ☇} then (r, [])
	if err != nil {
		panicErr := &pvm.ErrPanic{}
		if errors.Is(err, pvm.ErrOutOfGas) || errors.As(err, &panicErr) {
			return nil, nil, 0, err
		}
	}

	return result, ctxPair.Segments, uint64(remainingGas), err
}
