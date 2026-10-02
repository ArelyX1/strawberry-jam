package devnet

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// NetConf is the part of a network's description that is allowed to differ from
// machine to machine.
//
// The validator file says who exists and the genesis says what the chain is, and
// both of those have to be the same bytes everywhere or the machines are not on
// the same chain. Where each machine is, on the other hand, changes every time
// one moves, gets a new address, or sits behind a different NAT. Baking the
// address into the validator file means moving a machine means regenerating the
// file and shipping the new copy to everybody, and the moment one machine has an
// older copy than the rest the mesh is quietly half connected.
//
// So the addresses live here instead, in a file one machine edits for itself.
type NetConf struct {
	// Listen is where this node should bind its peer-to-peer socket. Empty means
	// take it from the validator file, which is what a single machine run wants.
	Listen string

	// Peers is the address of each validator, by index, overriding the one in the
	// validator file. A machine that cannot be reached at the address the file
	// names says so here, and that is one line on one machine.
	Peers map[uint16]string
}

// ParseNetConf reads a net conf from text.
//
// The format is deliberately dull, because it is going to be edited by hand on a
// remote machine while something is on fire:
//
//	listen = 100.71.247.117:30333
//
//	[peers]
//	0 = 100.71.247.117:30333
//	1 = 100.100.33.61:30333
//
// A machine's own address goes in listen; the addresses of the others go under
// peers, by validator index. Everything is optional, blank lines and lines
// starting with # are ignored.
//
// A mistake here is refused rather than carried on with, and that is the whole
// point of parsing it here instead of reading it loosely. A peer address that
// does not parse, a port that is not a port, an index that is not a number or a
// line that means nothing: each of those would leave a node running, connected to
// nobody, producing blocks on its own and looking perfectly healthy while the net
// is not a net.
func ParseNetConf(text string) (*NetConf, error) {
	conf := &NetConf{Peers: map[uint16]string{}}
	inPeers := false

	scanner := bufio.NewScanner(strings.NewReader(text))
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("netconf: line %d: a section is [name] and this one is not closed", lineNo)
			}
			section := strings.TrimSpace(line[1 : len(line)-1])
			if section != "peers" {
				return nil, fmt.Errorf("netconf: line %d: unknown section [%s], the only one is [peers]", lineNo, section)
			}
			inPeers = true
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("netconf: line %d: %q is not key = value", lineNo, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("netconf: line %d: %q has no value", lineNo, key)
		}

		if !inPeers {
			if key != "listen" {
				return nil, fmt.Errorf("netconf: line %d: outside [peers] the only key is listen, not %q", lineNo, key)
			}
			if conf.Listen != "" {
				return nil, fmt.Errorf("netconf: line %d: listen is already set", lineNo)
			}
			if err := checkHostPort(value); err != nil {
				return nil, fmt.Errorf("netconf: line %d: listen %q: %w", lineNo, value, err)
			}
			conf.Listen = value
			continue
		}

		index, err := strconv.ParseUint(key, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("netconf: line %d: %q is not a validator index: %w", lineNo, key, err)
		}
		if err := checkHostPort(value); err != nil {
			return nil, fmt.Errorf("netconf: line %d: validator %d: %w", lineNo, index, err)
		}
		if _, dup := conf.Peers[uint16(index)]; dup {
			return nil, fmt.Errorf("netconf: line %d: validator %d appears twice, and one of the two would be ignored", lineNo, index)
		}
		conf.Peers[uint16(index)] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("netconf: read failed: %w", err)
	}
	return conf, nil
}

// LoadNetConf reads and parses a net conf file. An empty path gives a nil conf,
// which is the single machine case and not an error.
func LoadNetConf(path string) (*NetConf, error) {
	if path == "" {
		return nil, nil
	}
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("netconf: %w", err)
	}
	return ParseNetConf(string(text))
}

func checkHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if host == "" {
		return fmt.Errorf("%q has no host", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q does not have a port between 1 and 65535", addr)
	}
	return nil
}

// ApplyAddrs puts the overrides into a list of validator addresses.
//
// The list is what the validator file said, by index. Only the entries named in
// the conf change, and the list that came in is not touched, so two nodes reading
// the same validator file and two different confs both start from the same place.
//
// An index the list has no room for is refused: the conf and the validator file
// disagree about how many validators there are, and quietly ignoring the extra
// one would leave a machine trying to reach a node that does not exist.
func (c *NetConf) ApplyAddrs(addrs []string) ([]string, error) {
	if c == nil {
		return addrs, nil
	}
	out := make([]string, len(addrs))
	copy(out, addrs)

	for index, addr := range c.Peers {
		if int(index) >= len(out) {
			return nil, fmt.Errorf("netconf: validator %d has an address, but the validator file only has %d", index, len(out))
		}
		if err := checkHostPort(addr); err != nil {
			return nil, fmt.Errorf("netconf: validator %d: %w", index, err)
		}
		out[index] = addr
	}
	return out, nil
}

// ListenAddr is where this node binds, and what the validator file says when the
// conf does not say otherwise.
func (c *NetConf) ListenAddr(index uint16, fileHost string, filePort int) (string, error) {
	if c != nil && c.Listen != "" {
		return c.Listen, nil
	}
	return net.JoinHostPort(fileHost, strconv.Itoa(filePort)), nil
}
