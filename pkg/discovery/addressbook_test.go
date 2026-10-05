package discovery

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Two machines derive the same identity from the same validator key, which is
// the entire reason a node can be found without having met anybody. If this
// fails, every node on the network becomes a stranger to every other one the
// moment it restarts with a fresh key.
func TestIdentityIsDerivedFromTheValidatorKey(t *testing.T) {
	clave := bytes.Repeat([]byte{7}, 32)

	a, err := IdentityFromValidatorKey(clave)
	if err != nil {
		t.Fatalf("no se pudo derivar la identidad: %v", err)
	}
	b, err := IdentityFromValidatorKey(clave)
	if err != nil {
		t.Fatalf("no se pudo derivar la identidad por segunda vez: %v", err)
	}

	ida, err := peer.IDFromPrivateKey(a)
	if err != nil {
		t.Fatalf("identificador invalido: %v", err)
	}
	idb, err := peer.IDFromPrivateKey(b)
	if err != nil {
		t.Fatalf("identificador invalido: %v", err)
	}
	if ida != idb {
		t.Errorf("la misma clave de validador dio dos identidades distintas: %s y %s", ida, idb)
	}

	otra := bytes.Repeat([]byte{8}, 32)
	c, err := IdentityFromValidatorKey(otra)
	if err != nil {
		t.Fatalf("no se pudo derivar la identidad de la otra clave: %v", err)
	}
	idc, err := peer.IDFromPrivateKey(c)
	if err != nil {
		t.Fatalf("identificador invalido: %v", err)
	}
	if ida == idc {
		t.Error("dos claves de validador distintas dieron la misma identidad, con lo cual " +
			"dos validadores distintos se找arian como si fueran el mismo")
	}
}

// The derived key must not be the validator key itself. The validator key signs
// blocks and its private half is never allowed off the machine, while this one
// exists to be published to the whole network.
func TestIdentityIsNotTheValidatorKey(t *testing.T) {
	clave := bytes.Repeat([]byte{3}, 32)

	priv, err := IdentityFromValidatorKey(clave)
	if err != nil {
		t.Fatalf("no se pudo derivar la identidad: %v", err)
	}

	raw, err := priv.Raw()
	if err != nil {
		t.Fatalf("no se pudo leer la clave derivada: %v", err)
	}
	if bytes.Contains(raw, clave) {
		t.Error("la identidad derivada contiene la clave de validador sin transformar")
	}
}

// A key of the wrong length is refused rather than hashed into something that
// looks like a valid identity.
func TestIdentityRefusesAWrongLengthKey(t *testing.T) {
	for _, n := range []int{0, 31, 33, 64} {
		if _, err := IdentityFromValidatorKey(make([]byte, n)); err == nil {
			t.Errorf("una clave de %d bytes deberia rechazarse", n)
		}
	}
}

// The address book is what lets a restart find the network again with nothing
// else available, so it has to survive a round trip and it has to come back in
// a form that can be dialled.
func TestAddressBookSurvivesAReload(t *testing.T) {
	dir := t.TempDir()

	otro, err := IdentityFromValidatorKey(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatalf("no se pudo derivar una identidad: %v", err)
	}
	id, err := peer.IDFromPrivateKey(otro)
	if err != nil {
		t.Fatalf("identificador invalido: %v", err)
	}

	addr := ma.StringCast("/ip4/100.64.1.7/tcp/40334")

	libro := loadAddressBook(dir)
	libro.remember(id, []ma.Multiaddr{addr})
	if err := libro.save(); err != nil {
		t.Fatalf("no se pudo escribir el libro: %v", err)
	}

	// A second load reads what the first one wrote, which is what a restart is.
	recargado := loadAddressBook(dir)
	todos := recargado.all()
	if len(todos) != 1 {
		t.Fatalf("tras recargar hay %d pares, y se esperaba 1", len(todos))
	}
	if todos[0].ID != id {
		t.Errorf("se recargo el par %s en vez de %s", todos[0].ID, id)
	}
	if len(todos[0].Addrs) != 1 || todos[0].Addrs[0].String() != addr.String() {
		t.Errorf("la direccion recargada no es la que se guardo: %v", todos[0].Addrs)
	}

	// The file exists on disk with a name that says what it is, so nobody clears
	// it as a cache and wonders later why the network has to be rebuilt.
	if _, err := os.Stat(filepath.Join(dir, knownPeersFile)); err != nil {
		t.Errorf("no se escribio %s: %v", knownPeersFile, err)
	}
}

// A peer seen again keeps the addresses it had as well as the new ones. A
// machine that answers on three addresses answers on all three, and forgetting
// the older ones means rediscovering them every time.
func TestAddressBookKeepsEveryAddressAcrossRepeatedSightings(t *testing.T) {
	dir := t.TempDir()
	libro := loadAddressBook(dir)

	priv, _ := IdentityFromValidatorKey(bytes.Repeat([]byte{2}, 32))
	id, _ := peer.IDFromPrivateKey(priv)

	libro.remember(id, []ma.Multiaddr{ma.StringCast("/ip4/10.0.0.1/tcp/40334")})
	libro.remember(id, []ma.Multiaddr{ma.StringCast("/ip6/::1/tcp/40334")})
	libro.remember(id, []ma.Multiaddr{ma.StringCast("/ip4/10.0.0.1/tcp/40334")})

	addrs := libro.addrs(id)
	if len(addrs) != 2 {
		t.Errorf("hay %d direcciones para el par, y se esperaban 2 sin repetir: %v", len(addrs), addrs)
	}
}

// An address nobody has answered on in a week is not worth dialling. A laptop
// that was on the cafe network last week is not there today, and keeping the
// entry only means paying for a handshake that cannot succeed.
func TestAddressBookForgetsWhatHasNotBeenSeenInAWeek(t *testing.T) {
	dir := t.TempDir()

	priv, _ := IdentityFromValidatorKey(bytes.Repeat([]byte{4}, 32))
	id, _ := peer.IDFromPrivateKey(priv)
	addr := "/ip4/10.0.0.9/tcp/40334"

	guardados := []knownPeer{{ID: id.String(), Addrs: []string{addr}, Seen: time.Now().Add(-8 * 24 * time.Hour)}}
	crudos := mustMarshal(t, guardados)
	if err := os.WriteFile(filepath.Join(dir, knownPeersFile), crudos, 0o600); err != nil {
		t.Fatalf("no se pudo escribir el libro: %v", err)
	}

	if todos := loadAddressBook(dir).all(); len(todos) != 0 {
		t.Errorf("una entrada de hace ocho dias sobrevvio: %v", todos)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("no se pudo codificar: %v", err)
	}
	return raw
}
