package handlers

import (
	"context"
	"testing"

	"github.com/eigerco/strawberry/internal/crypto/ed25519"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The handler holding announcers is built once for the process and handed to each
// connection as it arrives, and it keys them by the peer's public key. That key is
// the same before and after a reconnect, so an announcer found under a peer's key
// can belong to a connection that has since been replaced.
//
// Handing that one back means announcing into a stream the other end has already
// closed, which fails every time with "Application error 0x0 (remote)" and never
// recovers, because the entry is never replaced and so is found again on the next
// attempt. A peer that reconnected is left unable to tell anyone anything while the
// other end announces happily into a connection that is up.
func TestAnAnnouncerOnAGoneConnectionIsNotHandedBack(t *testing.T) {
	bh := NewBlockAnnouncementHandler(nil, nil)
	peerKey := ed25519.PublicKey(make([]byte, 32))

	// The connection the peer had before it went away and came back.
	goneConn, cancelGone := context.WithCancel(context.Background())
	goneOwn, cancelGoneOwn := context.WithCancel(goneConn)
	defer cancelGoneOwn()
	bh.Announcers[string(peerKey)] = &BlockAnnouncer{
		connCtx: goneConn, ctx: goneOwn, cancel: cancelGoneOwn,
	}
	// The peer reconnects: a new connection, a new context, the same key.
	liveConn, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()

	// Looking the peer up by key alone hands back the announcer belonging to the
	// connection that is gone.
	assert.NotNil(t, bh.Announcers[string(peerKey)],
		"the stale entry is still there, which is why this had to be caught")

	announcer, found := bh.LiveAnnouncer(peerKey, liveConn)
	assert.False(t, found,
		"an announcer on a connection that has gone must not be handed back, because "+
			"announcing into its stream fails every time and the entry is never replaced")
	assert.Nil(t, announcer)

	// And it is gone for good, so the next caller is not handed it either. Leaving
	// it in the map is what made the failure last as long as the peer stayed
	// connected.
	cancelGone()
	assert.NotContains(t, bh.Announcers, string(peerKey),
		"the stale announcer must be taken out of the map and cancelled, not just ignored")

	// The next announcement opens a fresh announcer on the connection that is here.
	fresh := &BlockAnnouncer{connCtx: liveConn, ctx: liveConn}
	bh.Announcers[string(peerKey)] = fresh
	got, ok := bh.LiveAnnouncer(peerKey, liveConn)
	require.True(t, ok, "an announcer on the current connection is the one to reuse")
	assert.Same(t, fresh, got)
}

// Two connections to the same peer at once, as when both ends dial each other and
// one of the two is then dropped. The announcer on the surviving connection is the
// one to keep, whichever of the two asked first, so asking does not depend on
// arrival order.
func TestLiveAnnouncerDoesNotDependOnArrivalOrder(t *testing.T) {
	peerKey := ed25519.PublicKey(make([]byte, 32))

	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	second, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()

	// The announcer that already exists is on the second connection.
	existing := &BlockAnnouncer{connCtx: second, ctx: second, cancel: cancelSecond}

	bh := NewBlockAnnouncementHandler(nil, nil)
	bh.Announcers[string(peerKey)] = existing

	// Asked on the connection that holds it, it comes back.
	got, ok := bh.LiveAnnouncer(peerKey, second)
	require.True(t, ok)
	assert.Same(t, existing, got)

	// Asked on the other one, it does not, and it is cleared so the connection that
	// is being kept can put its own announcer in its place.
	_, ok = bh.LiveAnnouncer(peerKey, first)
	assert.False(t, ok,
		"an announcer belongs to the connection it was opened on and to no other")
	assert.NotContains(t, bh.Announcers, string(peerKey))
}

// An announcer whose own context has been cancelled is finished with whatever
// happened to its connection, and must not be handed back either.
func TestAFinishedAnnouncerIsNotHandedBack(t *testing.T) {
	bh := NewBlockAnnouncementHandler(nil, nil)
	peerKey := ed25519.PublicKey(make([]byte, 32))

	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()
	ownCtx, cancelOwn := context.WithCancel(connCtx)
	cancelOwn()

	bh.Announcers[string(peerKey)] = &BlockAnnouncer{
		connCtx: connCtx, ctx: ownCtx, cancel: cancelOwn,
	}

	// Same connection, but the announcer is finished, so there is no stream to
	// announce on.
	_, found := bh.LiveAnnouncer(peerKey, connCtx)
	assert.False(t, found, "a finished announcer has no stream left to announce on")
	assert.NotContains(t, bh.Announcers, string(peerKey))
}

// Which connection an announcer belongs to is decided by the connection, not by
// how old it is. Two connections to one peer can both be open for a moment, and
// each of them has to get the announcer that is on it, whichever arrived first, so
// that neither gets the other's stream and neither is left announcing into a
// connection the other end has closed.
func TestAnnouncerIsChosenByConnectionAndNotByAge(t *testing.T) {
	peerKey := ed25519.PublicKey(make([]byte, 32))

	older, cancelOlder := context.WithCancel(context.Background())
	defer cancelOlder()
	newer, cancelNewer := context.WithCancel(context.Background())
	defer cancelNewer()

	// Each connection holds an announcer of its own, and both are open.
	olderAnnouncer := &BlockAnnouncer{connCtx: older, ctx: older, cancel: cancelOlder}
	newerAnnouncer := &BlockAnnouncer{connCtx: newer, ctx: newer, cancel: cancelNewer}

	// The older connection asks first and gets the older announcer, so being first
	// is not what decides it.
	bh := NewBlockAnnouncementHandler(nil, nil)
	bh.Announcers[string(peerKey)] = olderAnnouncer
	got, found := bh.LiveAnnouncer(peerKey, older)
	require.True(t, found)
	assert.Same(t, olderAnnouncer, got,
		"an announcer belongs to the connection it was opened on, and being asked "+
			"first does not make a connection take another one's")

	// The newer connection has taken over the peer, and brings its own announcer
	// with it. The older one is then on a connection that is going away, and is
	// dropped so that announcing does not go on using it.
	bh.Announcers[string(peerKey)] = newerAnnouncer
	got, found = bh.LiveAnnouncer(peerKey, newer)
	require.True(t, found)
	assert.Same(t, newerAnnouncer, got,
		"the connection that is being kept announces on its own stream, not on the "+
			"one belonging to the connection that was dropped")
}
