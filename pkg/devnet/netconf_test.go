package devnet

import (
	"testing"
)

func TestParseNetConfReadsAListenAndAPeerList(t *testing.T) {
	conf, err := ParseNetConf(`
# esta maquina escucha aqui
listen = 100.71.247.117:30333

[peers]
0 = 100.71.247.117:30333
1 = 100.100.33.61:30333
2 = [::1]:30334
`)
	if err != nil {
		t.Fatal(err)
	}
	if conf.Listen != "100.71.247.117:30333" {
		t.Errorf("listen = %q", conf.Listen)
	}
	if len(conf.Peers) != 3 {
		t.Fatalf("peers = %d, want 3", len(conf.Peers))
	}
	if got := conf.Peers[2]; got != "[::1]:30334" {
		t.Errorf("peer 2 = %q, want [::1]:30334", got)
	}
}

// A node with no conf is a single machine node, and that has to keep working
// without a file anywhere in sight.
func TestNoConfLeavesEverythingWhereTheValidatorFilePutIt(t *testing.T) {
	conf, err := LoadNetConf("")
	if err != nil || conf != nil {
		t.Fatalf("sin conf: %v, %v; quiero nil, nil", conf, err)
	}
	got, err := conf.ListenAddr(0, "127.0.0.1", 30333)
	if err != nil || got != "127.0.0.1:30333" {
		t.Errorf("sin conf escucha en %q (%v), quiero lo que dice el fichero", got, err)
	}
}

// Each of these is a way the file can be wrong. They all have to be refused at
// startup rather than producing a node that runs, connects to nobody, and looks
// healthy while the net is not a net.
func TestAMistakeInNetConfIsRefusedRatherThanCarriedOnWith(t *testing.T) {
	cases := []struct {
		name, text, why string
	}{
		{"puerto que no es puerto", "listen = 1.2.3.4:no-puerto", "puerto"},
		{"puerto fuera de rango", "listen = 1.2.3.4:70000", "rango"},
		{"sin puerto", "listen = 1.2.3.4", "host:port"},
		{"sin host", "listen = :30333", "host"},
		{"clave desconocida", "listen = 1.2.3.4:30333\norigen = 5.6.7.8", "listen"},
		{"seccion desconocida", "[vecinos]\n0 = 1.2.3.4:30333", "[peers]"},
		{"seccion sin cerrar", "[peers\n0 = 1.2.3.4:30333", "cerr"},
		{"indice que no es indice", "[peers]\nprimero = 1.2.3.4:30333", "indice"},
		{"linea sin igual", "listen 1.2.3.4:30333", "key = value"},
		{"valor vacio", "listen =", "sin valor"},
		{"mismo indice dos veces", "[peers]\n1 = 1.2.3.4:30333\n1 = 5.6.7.8:30333", "dos veces"},
		{"listen repetido", "listen = 1.2.3.4:30333\nlisten = 5.6.7.8:30333", "ya"},
		{"direccion de peer rota", "[peers]\n1 = 5.6.7.8", "host:port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseNetConf(c.text); err == nil {
				t.Errorf("aceptado algo que no es: %s", c.why)
			}
		})
	}
}

// A peer address for a validator that is not in the file is a mistake worth
// stopping for: it means the file and the conf disagree about how big the network
// is, and one of the two is out of date.
func TestAPeerForAValidatorThatDoesNotExistIsRefused(t *testing.T) {
	conf, err := ParseNetConf("[peers]\n5 = 1.2.3.4:30333")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conf.ApplyAddrs([]string{"a:1", "b:2", "c:3"}); err == nil {
		t.Error("aceptada una direccion para el validador 5 con solo 3 validadores en el fichero")
	}
}

// An address in the conf replaces that validator's address and nothing else: the
// rest of the network is where the validator file said it was.
func TestApplyAddrsReplacesOnlyTheValidatorsNamed(t *testing.T) {
	base := []string{"10.0.0.1:30333", "10.0.0.2:30333", "10.0.0.3:30333"}
	conf, err := ParseNetConf("[peers]\n1 = 100.100.33.61:30333")
	if err != nil {
		t.Fatal(err)
	}
	got, err := conf.ApplyAddrs(base)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.1:30333", "100.100.33.61:30333", "10.0.0.3:30333"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("validador %d en %s, quiero %s", i, got[i], want[i])
		}
	}
	// The list that came in is not touched, so a second node reading the same
	// validator file starts from the same place whatever the first one did.
	if base[1] != "10.0.0.2:30333" {
		t.Errorf("la lista original quedo en %s, deberia seguir en 10.0.0.2:30333", base[1])
	}
}

func TestListenAddrUsesTheConfOverTheFile(t *testing.T) {
	conf, err := ParseNetConf("listen = 100.71.247.117:30333")
	if err != nil {
		t.Fatal(err)
	}
	got, err := conf.ListenAddr(0, "127.0.0.1", 30333)
	if err != nil {
		t.Fatal(err)
	}
	if got != "100.71.247.117:30333" {
		t.Errorf("escucha en %q, quiero lo del conf", got)
	}
}

func TestHostPortCheckAcceptsWhatItShould(t *testing.T) {
	for _, good := range []string{"1.2.3.4:30333", "[::1]:30333", "host.example:1", "10.0.0.5:65535"} {
		if err := checkHostPort(good); err != nil {
			t.Errorf("%q rechazado: %v", good, err)
		}
	}
}
