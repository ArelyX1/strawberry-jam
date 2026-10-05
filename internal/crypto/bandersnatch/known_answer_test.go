package bandersnatch

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/eigerco/strawberry/internal/crypto"
)

// These are known answer values, and they exist because this library is built two
// ways. The asm feature uses sha2-asm, which does not compile for Windows, so
// Windows gets the same code without it. Both were compared byte for byte and
// agreed, and that is what makes the portable build safe to ship instead of a
// node that speaks a different variant of the protocol on Windows.
//
// Signing and then verifying with the same build does not prove that. Two
// implementations that disagree with each other are each perfectly happy to
// agree with themselves, and the disagreement would only surface as a
// signature that a validator on another machine rejects, which is a failure
// that looks like somebody cheating.
//
// So the expected bytes are written down here. If a change to the Rust library,
// to the FFI signatures or to how the pointers are passed changes any output,
// this fails here rather than in a block that never validates.
func TestKnownAnswerValues(t *testing.T) {
	const (
		wantPrivate = "b4820fcc3df83d0371e9b435ff1842d53683a7a9785e5c0b062189b1dfddc009"
		wantPublic  = "ccb6e8cb02d304ea7cba82db4307d3acbe0e57c8f93e7f373e058bcc36427e2e"
		wantSig     = "80e4b35f8f6a791a725f5706f4178b0c498a780e78efb223bc6b1c045481986a57" +
			"845d6e813849fa7939d64122c05548c1a8429f830f259418ec7a15f32945116d0" +
			"5cb51c81d526a661c31c8a394964b058ff29a97cfc5fbe54d6f3efd44df0e"
		wantOutput = "c063c540ee84c2d849f52c2172de3d0109e89ec744bedff4ab729404cdaedb3e"
	)

	// The same seed and the same input the expected values were produced with.
	var seed crypto.BandersnatchSeedKey
	for i := range seed {
		seed[i] = byte(i*7 + 3)
	}

	input := make([]byte, 64)
	for i := range input {
		input[i] = byte(i * 3)
	}
	aux := []byte{1, 2, 3, 4, 5}

	private, err := NewPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("no se pudo derivar la clave privada: %v", err)
	}
	if got := hex.EncodeToString(private[:]); got != wantPrivate {
		t.Errorf("clave privada distinta de la conocida:\n  tiene  %s\n  quiere %s", got, wantPrivate)
	}

	public, err := Public(private)
	if err != nil {
		t.Fatalf("no se pudo derivar la clave publica: %v", err)
	}
	if got := hex.EncodeToString(public[:]); got != wantPublic {
		t.Errorf("clave publica distinta de la conocida:\n  tiene  %s\n  quiere %s", got, wantPublic)
	}

	signature, err := Sign(private, input, aux)
	if err != nil {
		t.Fatalf("no se pudo firmar: %v", err)
	}
	if got := hex.EncodeToString(signature[:]); got != wantSig {
		t.Errorf("firma distinta de la conocida:\n  tiene  %s\n  quiere %s", got, wantSig)
	}

	valid, output := Verify(public, input, aux, signature)
	if !valid {
		t.Fatal("la firma recien creada no verifica")
	}
	if got := hex.EncodeToString(output[:]); got != wantOutput {
		t.Errorf("hash de salida distinto del conocido:\n  tiene  %s\n  quiere %s", got, wantOutput)
	}
}

// The lengths are passed to Rust as size_t. They used to be declared as cgo's
// C.size_t, which made cgo a build dependency and with it a cross compiler for C
// on every target. They are plain uint64 now, which is the same width on the
// 64 bit targets this is built for.
//
// A wrong length is silent in the sense that matters here: Rust reads
// seedLength bytes from the pointer it was given, so a length that is too large
// reads past the end of the slice and produces a key from whatever was in memory
// after it, rather than failing. This is the cheapest place to notice that the
// lengths are being passed.
func TestInputAndAuxLengths(t *testing.T) {
	private, err := NewPrivateKeyFromSeed(crypto.BandersnatchSeedKey{})
	if err != nil {
		t.Fatalf("una semilla de ceros deberia ser valida: %v", err)
	}
	public, err := Public(private)
	if err != nil {
		t.Fatalf("no se pudo derivar la clave publica: %v", err)
	}

	// Una semilla de ceros produce una clave, y tiene que producir una clave
	// distinta de cualquier otra, o todas las semillas darian lo mismo.
	if other, err := NewPrivateKeyFromSeed(crypto.BandersnatchSeedKey{1}); err != nil {
		t.Fatalf("no se pudo derivar la segunda clave: %v", err)
	} else if bytes.Equal(private[:], other[:]) {
		t.Error("dos semillas distintas produjeron la misma clave privada")
	}

	aux := []byte{9}

	// Longitudes que van y vienen. La de 4096 bytes esta porque es el caso donde
	// un puntero y una longitud que no coinciden dan una firma distinta sin
	// fallar, y porque en una maquina de pruebas no es un problema pedir ese
	// tamano y si lo es en un nodo real.
	for _, tam := range []int{1, 2, 31, 32, 33, 1024, 4096} {
		input := make([]byte, tam)
		for i := range input {
			input[i] = byte(i)
		}
		signature, err := Sign(private, input, aux)
		if err != nil {
			t.Fatalf("firmar %d bytes fallo: %v", tam, err)
		}
		if valid, _ := Verify(public, input, aux, signature); !valid {
			t.Fatalf("la firma de %d bytes no verifica", tam)
		}
	}

	// Ni la entrada ni la aux pueden ir vacias, y la libreria de Rust lo rechaza
	// en vez de firmar el mensaje vacio. Se deja anotado porque el rechazo viene
	// de ahi y no de este enlace: un cambio en Rust que lo aceptara dejaria estas
	// pruebas sin validar nada, y firmaria el mensaje vacio.
	for _, tc := range []struct {
		nombre     string
		input, aux []byte
	}{
		{"entrada vacia", nil, aux},
		{"entrada vacia con aux vacia", nil, nil},
		{"aux vacia", []byte{1}, nil},
	} {
		if _, err := Sign(private, tc.input, tc.aux); err == nil {
			t.Errorf("%s deberia rechazarse", tc.nombre)
		}
	}
}

// A signature made for one input must not verify against another. This is what
// the lengths being passed correctly protects: if the length were wrong, the
// bytes past the input would leak into the signed message and a signature could
// cover more than it should.
func TestSignatureDoesNotVerifyForDifferentInput(t *testing.T) {
	var seed crypto.BandersnatchSeedKey
	for i := range seed {
		seed[i] = byte(i*7 + 3)
	}
	private, err := NewPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("no se pudo derivar la clave privada: %v", err)
	}
	public, err := Public(private)
	if err != nil {
		t.Fatalf("no se pudo derivar la clave publica: %v", err)
	}

	one := bytes.Repeat([]byte{1}, 32)
	two := bytes.Repeat([]byte{2}, 32)

	aux := []byte{9}

	signature, err := Sign(private, one, aux)
	if err != nil {
		t.Fatalf("no se pudo firmar: %v", err)
	}

	if valid, _ := Verify(public, two, aux, signature); valid {
		t.Error("una firma sobre otra entrada verifica, no deberia")
	}
}
