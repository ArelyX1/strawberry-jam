package handlers

import (
	"context"

	"github.com/eigerco/strawberry/internal/crypto/ed25519"

	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/store"
	"github.com/eigerco/strawberry/pkg/network"
	"github.com/eigerco/strawberry/pkg/serialization/codec/jam"

	"github.com/quic-go/quic-go"
)

// connState represents the current state of a block announcement connection.
// It tracks the progress of the handshake before normal operation begins.
type connState int

const (
	headerSize                   = 297  // Size in bytes of a serialized block header
	SendingHandshake   connState = iota // Initial state: sending handshake to peer
	ReceivingHandshake                  // Awaiting handshake response from peer
	Ready                               // Handshake completed, ready for announcements
)

// BlockRequestor defines an interface for requesting blocks from peers.
type BlockRequestor interface {
	RequestBlocks(ctx context.Context, hash crypto.Hash, ascending bool, maxBlocks uint32, peerKey ed25519.PublicKey) ([]block.Block, error)
}

// BlockAnnouncementHandler implements the UP 0 block announcement protocol from the JAMNP spec.
// It maintains a map of active block announcers for each connected peer and handles
// new announcement streams according to protocol rules for Unique Persistent streams.
type BlockAnnouncementHandler struct {
	*chain.BlockService                            // Provides access to the local node's chain state
	mu                  sync.RWMutex               // Protects the Announcers map from concurrent access
	Announcers          map[string]*BlockAnnouncer // Maps peer keys to their respective announcers
	requestor           BlockRequestor             // Used to request blocks after announcements
	onBlockReceiveHooks []BlockReceiveHook         // hooks that trigger on receiving a new block, needed to start processes like assurance and auditing
}

// NewBlockAnnouncementHandler creates a new handler with the provided block service
// and block requestor. The block service provides access to the node's chain state,
// while the requestor allows fetching blocks from peers after receiving announcements.
func NewBlockAnnouncementHandler(bs *chain.BlockService, requestor BlockRequestor) *BlockAnnouncementHandler {
	return &BlockAnnouncementHandler{
		BlockService: bs,
		Announcers:   make(map[string]*BlockAnnouncer),
		requestor:    requestor,
	}
}

type BlockReceiveHook func(ctx context.Context, block block.Block)

// announceBlockBatch is how many blocks are asked for when a block is announced:
// the announced one and the stretch behind it, so a node that is behind is not one
// block behind for the rest of its life.
//
// One block per announcement is what made the chain stop. The author of a timeslot
// writes its block at the start of that timeslot, and the node that has to build
// on it wants it at the start of the next one, so a single block per announcement
// leaves the follower behind every time and leaves the next author waiting for a
// block nobody is going to produce.
const announceBlockBatch = 32

// BlockAnnouncer manages a single UP 0 block announcement stream with a peer.
// It handles the initial handshake, tracking peer's chain state (finalized blocks and leaves),
// and bidirectional exchange of block announcements according to the protocol rules.
type BlockAnnouncer struct {
	*chain.BlockService                                  // Access to local chain state
	peerFinalized       chain.LatestFinalized            // Peer's latest finalized block
	mu                  sync.RWMutex                     // Protects the announced and peerLeaves maps
	peerLeaves          map[crypto.Hash]jamtime.Timeslot // Peer's leaf blocks (blocks with no children)
	announced           map[crypto.Hash]*block.Header    // Blocks we've announced to this peer TODO: Cleanup mehcanism
	stream              *quic.Stream                     // The QUIC stream for this connection
	ctx                 context.Context                  // Context for cancellation
	connCtx             context.Context                  // Context of the connection this announcer belongs to
	cancel              context.CancelFunc               // Function to cancel the context
	stateLock           sync.Mutex                       // Protects the connection state
	state               connState                        // Current state of the connection
	handshakeCh         chan struct{}                    // Signals completion of handshake send
	readyCh             chan struct{}                    // Signals the announcer is ready for operation
	sendCh              chan []byte                      // Channel for outgoing messages
	receiveCh           chan []byte                      // Channel for incoming messages
	requestor           BlockRequestor                   // Used to request blocks after announcements
	peerKey             ed25519.PublicKey                // Ed25519 key of the connected peer
	onBlockReceiveHooks []BlockReceiveHook               // hooks that trigger on receiving a new block, needed to start processes like assurance and auditing
}

// NewBlockAnnouncer creates a new announcer for a given stream and peer.
// It initializes the announcer in the SendingHandshake state and registers
// it in the handler's Announcers map using the peer's Ed25519 key.
func (bh *BlockAnnouncementHandler) NewBlockAnnouncer(bs *chain.BlockService, ctx context.Context, stream *quic.Stream, peerKey ed25519.PublicKey) *BlockAnnouncer {
	announcerCtx, cancel := context.WithCancel(ctx)
	bh.mu.Lock()
	defer bh.mu.Unlock()

	ba := &BlockAnnouncer{
		BlockService:        bs,
		stream:              stream,
		ctx:                 announcerCtx,
		connCtx:             ctx,
		cancel:              cancel,
		state:               SendingHandshake,
		handshakeCh:         make(chan struct{}),
		readyCh:             make(chan struct{}),
		sendCh:              make(chan []byte, 10),
		receiveCh:           make(chan []byte, 10),
		requestor:           bh.requestor,
		peerKey:             peerKey,
		announced:           make(map[crypto.Hash]*block.Header),
		peerLeaves:          make(map[crypto.Hash]jamtime.Timeslot),
		onBlockReceiveHooks: bh.onBlockReceiveHooks,
	}
	bh.Announcers[string(peerKey)] = ba
	return ba
}

// HandleStream processes a new UP 0 stream according to the JAMNP requirements.
// Since UP streams should be unique per connection, it handles the case where a stream
// already exists for the peer by keeping only the stream with the higher stream ID.
// This implements the spec rule: "If exists: Close old connection, cleanup peer state."
func (bh *BlockAnnouncementHandler) HandleStream(ctx context.Context, stream *quic.Stream, peerKey ed25519.PublicKey) error {
	bh.mu.RLock()
	existingAnnouncer, exists := bh.Announcers[string(peerKey)]
	bh.mu.RUnlock()

	// An announcer whose connection is gone is not an announcer, and this map
	// outlives every connection: the handler is created once for the process and
	// handed to each connection as it arrives. So a peer that disconnects and
	// comes back leaves its old entry here, still holding the stream ids of a
	// connection that no longer exists.
	//
	// Comparing stream ids against that stale entry is what breaks a returning
	// peer. Ids are only ordered within one connection, and the new stream starts
	// counting from zero again, so the dead entry looks like the winner and the
	// arriving peer gets reset with code 0, which is exactly the
	// "Application error 0x0 (remote)" that a node coming back from being away
	// used to hit. The context is the connection's own, cancelled when the
	// connection closes, so it tells the two apart.
	// Which connection is the existing announcer on? The same one, or a
	// connection that has since gone. This matters more than whether it still
	// looks alive: when a peer restarts, it reconnects immediately, and the
	// connection it left behind can still be up on this side for the length of
	// the idle timeout. Checking whether the old announcer's context is cancelled
	// is therefore no good here, because the reconnect usually beats the
	// teardown, and the arriving peer gets refused exactly as before.
	//
	// So: same connection, then the stream id rule decides, which is what it is
	// for. Different connection, then there is nothing to compare. The ids of the
	// old connection say nothing about the new one, whose streams count from zero
	// again, and comparing them makes the dead announcer look like the winner and
	// resets the newcomer with code 0.
	if exists && existingAnnouncer.connCtx != ctx {
		bh.mu.Lock()
		if bh.Announcers[string(peerKey)] == existingAnnouncer {
			delete(bh.Announcers, string(peerKey))
		}
		bh.mu.Unlock()
		existingAnnouncer.cancel()
		exists = false
	}

	// And one on this same connection that is already finished with is not a
	// candidate to win an id comparison either.
	if exists && existingAnnouncer.ctx.Err() != nil {
		bh.mu.Lock()
		if bh.Announcers[string(peerKey)] == existingAnnouncer {
			delete(bh.Announcers, string(peerKey))
		}
		bh.mu.Unlock()
		existingAnnouncer.cancel()
		exists = false
	}

	if exists {
		// Compare stream IDs - keep the one with greater ID as specified in the JAM protocol
		// "the stream with the greatest ID should be kept, and the other streams should be reset/stopped"
		if stream.StreamID() > existingAnnouncer.stream.StreamID() {
			// Cancel read/write on old stream
			existingAnnouncer.stream.CancelRead(quic.StreamErrorCode(0))
			existingAnnouncer.stream.CancelWrite(quic.StreamErrorCode(0))
			// Cancel existing announcer's context to stop its goroutines
			existingAnnouncer.cancel()
			// Create new announcer with the new stream
			handler := bh.NewBlockAnnouncer(bh.BlockService, ctx, stream, peerKey)
			return handler.Start()
		} else {
			// Cancel read/write on new stream as it has lower ID
			stream.CancelRead(quic.StreamErrorCode(0))
			stream.CancelWrite(quic.StreamErrorCode(0))
			return nil
		}
	}

	// If no announcer exists yet, create new one and start handshake process
	handler := bh.NewBlockAnnouncer(bh.BlockService, ctx, stream, peerKey)
	return handler.Start()
}

// AddOnBlockReceiveHook add on block received hook, required to kick off other processes like assurance and auditing
func (bh *BlockAnnouncementHandler) AddOnBlockReceiveHook(hook BlockReceiveHook) {
	bh.onBlockReceiveHooks = append(bh.onBlockReceiveHooks, hook)
}

// Done reports whether the connection this announcer lives on is gone, which
// means the stream under it is dead and the announcer has to be replaced rather
// than reused. Reusing one of these is how a node that stayed up while its peer
// restarted would keep announcing into a closed stream forever.
func (ba *BlockAnnouncer) Done() bool {
	return ba.ctx.Err() != nil
}

// Start initiates the block announcement protocol by triggering the handshake process.
// It waits for the handshake to complete before returning to ensure the announcer
// is ready for operation. Returns an error if the context is canceled before completion.
func (ba *BlockAnnouncer) Start() error {
	// First handle handshake
	go ba.handleHandshake()

	// Wait for ready state before returning
	select {
	case <-ba.readyCh:
		return nil
	case <-ba.ctx.Done():
		log.Println("BlockAnnouncer Start() - Context canceled before handshake completed")
		return ba.ctx.Err()
	}
}

// SendAnnouncement sends a block announcement to the peer.
// It first checks if the block should be announced, then serializes the header along with
// our latest finalized block information as required by the protocol.
// Format: Block Header + Finalized Block Hash + Finalized Block Slot
func (ba *BlockAnnouncer) SendAnnouncement(header *block.Header) error {
	// Wait for ready state before attempting to send
	select {
	case <-ba.readyCh:
		// Continue with send
	case <-ba.ctx.Done():
		return ba.ctx.Err()
	}
	// Check if we should announce this block according to protocol rules
	shouldAnnounce, err := ba.shouldAnnounce(header)
	if err != nil {
		return fmt.Errorf("check if should announce: %w", err)
	}
	if !shouldAnnounce {
		return nil
	}
	// Format: header + finalized(hash + slot)
	content := make([]byte, 333) // 297 for header + finalHash 36 for finalized, + 4 for slot
	hb, err := header.Bytes()
	if err != nil {
		return fmt.Errorf("failed to marshal header: %w", err)
	}
	copy(content[0:], hb)
	copy(content[297:], ba.LatestFinalized.Hash[:])
	binary.LittleEndian.PutUint32(content[329:], uint32(ba.LatestFinalized.TimeSlotIndex))
	// Send the message through the channel
	select {
	case ba.sendCh <- content:
		// Track this block as announced
		hash, _ := header.Hash() // Error already checked in shouldAnnounce
		ba.mu.Lock()
		ba.announced[hash] = header
		ba.mu.Unlock()
		return nil
	case <-ba.ctx.Done():
		return ba.ctx.Err()
	}
}

// shouldAnnounce implements the JAMNP rules for when a block should be announced:
// - The block must be a descendant of the latest finalized block
// - We should not announce blocks we've already announced
// - We should not announce a block if we've already announced one of its descendants
func (ba *BlockAnnouncer) shouldAnnounce(h *block.Header) (bool, error) {
	ba.mu.RLock()
	defer ba.mu.RUnlock()
	hash, err := h.Hash()
	if err != nil {
		return false, fmt.Errorf("hash header: %w", err)
	}

	// Check if the block is a descendant of the latest finalized block
	isDescendant, err := ba.IsDescendantOfFinalized(h)
	if err != nil {
		return false, fmt.Errorf("checking if block is descendant of finalized: %w", err)
	}
	if !isDescendant {
		return false, nil
	}

	// Check if we've already announced this block or one of its descendants
	// This implements the JAMNP rules:
	// - "A descendant of the block is announced instead"
	// - "The block, or a descendant of the block, has been announced by the other side of the stream"
	for announcedHash, announced := range ba.announced {
		if hash == announcedHash {
			// Already announced this exact block
			return false, nil
		}
		if hash == announced.ParentHash {
			// Already announced a child of this block
			return false, nil
		}
	}
	return true, nil
}

// handleHandshake manages the initial handshake exchange required by the UP 0 protocol.
// It first sends our handshake, then waits for the peer's handshake. Once both are
// complete, it transitions to the Ready state and starts the send/receive loops.
func (ba *BlockAnnouncer) handleHandshake() {
	if err := ba.sendHandshake(); err != nil {
		return
	}

	close(ba.handshakeCh)
	if err := ba.receiveHandshake(); err != nil {
		log.Println("[ERROR] (ba *BlockAnnouncer) receiveHandshake() - Handshake failed:", err)
		return
	}

	ba.stateLock.Lock()
	ba.state = Ready
	ba.stateLock.Unlock()
	close(ba.readyCh)

	// Start processing announcements
	go ba.sendLoop()
	go ba.receiveLoop()
}

// receiveHandshake waits for and processes the peer's handshake message,
// which contains the peer's latest finalized block and leaf blocks.
func (ba *BlockAnnouncer) receiveHandshake() error {
	ba.stateLock.Lock()
	ba.state = ReceivingHandshake
	ba.stateLock.Unlock()

	msg, err := ReadMessageWithContext(ba.ctx, ba.stream)
	if err != nil {
		return fmt.Errorf("failed to receive handshake: %w", err)
	}

	return ba.processHandshake(msg.Content)
}

// sendHandshake sends our handshake message to the peer, containing our
// latest finalized block and all our known leaf blocks as required by the protocol.
func (ba *BlockAnnouncer) sendHandshake() error {
	ba.mu.RLock()
	content := serializeHandshake(ba.LatestFinalized, ba.KnownLeaves)
	ba.mu.RUnlock()

	return WriteMessageWithContext(ba.ctx, ba.stream, content)
}

// sendLoop continuously monitors the send channel for outgoing messages
// and writes them to the stream. It terminates when the context is canceled.
func (ba *BlockAnnouncer) sendLoop() {
	// A loop that stops has to take the announcer down with it. The stream can
	// die while the connection lives on, and an announcer that is still there
	// after its stream is gone is worse than no announcer: SendAnnouncement keeps
	// handing messages to a channel nobody is reading and reports success, so
	// the node believes it has told everyone about every block it wrote and
	// nobody downstream ever hears. Marking it done is what lets the next
	// announcement open a stream that works.
	defer ba.cancel()

	for {
		select {
		case <-ba.ctx.Done():
			log.Println("(ba *BlockAnnouncer) sendLoop() - Context canceled, stopping")
			return
		case msg := <-ba.sendCh:
			if ba.stream == nil {
				log.Println("(ba *BlockAnnouncer) sendLoop() - Stream is nil, cannot send message")
				return
			}

			if err := WriteMessageWithContext(ba.ctx, ba.stream, msg); err != nil {
				log.Println("(ba *BlockAnnouncer) sendLoop() - Failed to send message:", err)
				return
			}
		}
	}
}

// receiveLoop continuously reads messages from the stream and processes
// them as block announcements. It terminates when an error occurs or
// the context is canceled.
func (ba *BlockAnnouncer) receiveLoop() {
	defer ba.cancel()

	for {
		select {
		case <-ba.ctx.Done():
			return
		default:
			msg, err := ReadMessageWithContext(ba.ctx, ba.stream)
			if err != nil {
				log.Println("(ba *BlockAnnouncer) receiveLoop() - Failed to read message:", err)
				return
			}
			if err := ba.processAnnouncement(msg.Content); err != nil {
				log.Println("(ba *BlockAnnouncer) receiveLoop() - could not process announcement:", err)
				continue
			}
		}
	}
}

// processAnnouncement handles a block announcement received from the peer.
// It extracts the header and finalized block information, validates the announcement,
// stores the new header, and requests the full block if needed.
func (ba *BlockAnnouncer) processAnnouncement(content []byte) error {
	if len(content) < 333 { // Minimum size: header(297) + finalized(36)
		return fmt.Errorf("announcement message too short")
	}

	// Extract header
	var header block.Header
	peerFinalized := chain.LatestFinalized{}

	err := jam.Unmarshal(content[:headerSize], &header)
	if err != nil {
		return fmt.Errorf("unmarshal header: %w", err)
	}
	copy(peerFinalized.Hash[:], content[headerSize:headerSize+32])

	peerFinalized.TimeSlotIndex = jamtime.Timeslot(binary.LittleEndian.Uint32(content[headerSize+32:]))

	// Validate the announcement: announced block must be after peer's finalized block
	if header.TimeSlotIndex <= peerFinalized.TimeSlotIndex {
		return fmt.Errorf("announced block slot (%d) not after peer's finalized slot (%d)", header.TimeSlotIndex, peerFinalized.TimeSlotIndex)
	}

	// Check if we know the peer's finalized block
	_, err = ba.Store.GetHeader(peerFinalized.Hash)
	if err != nil {
		if errors.Is(err, store.ErrHeaderNotFound) {
			// We don't know peer's finalized block - might need to sync
			fmt.Printf("Warning: Peer's finalized block %x unknown", peerFinalized.Hash)
		} else {
			return fmt.Errorf("checking for peer's finalized block in our db: %w", err)
		}
	}

	// Process the new header according to our chain rules
	// A header whose ancestors are missing is the ordinary state of a node that
	// has fallen behind, not a fault: the header is kept as a handle and the
	// blocks behind it are asked for, so the chain can be walked again.
	// Failing here would throw that away and, because the caller stops
	// listening when this returns an error, would also cost the node the
	// stream it learns on.
	// A header whose ancestors are missing is the ordinary state of a node that has
	// fallen behind, not a fault, and it is kept as the handle to go and fetch what
	// is behind it. It is not a reason to stop here either: the block itself is
	// still wanted, and going on to ask for it is the whole point of keeping the
	// header. Returning early instead left every block that arrived before its
	// ancestors unfetched, so the node learned of a head it could never walk to.
	overAGap := false
	if err = ba.HandleNewHeader(&header); err != nil {
		if !chain.IsMissingHeader(err) {
			return fmt.Errorf("process new block: %w", err)
		}
		overAGap = true
	}

	if overAGap {
		log.Printf("Announced block at slot %d is ahead of this node; going to fetch it", header.TimeSlotIndex)
	}

	// Request the full block using the CE 128 protocol
	h, err := header.Hash()
	if err != nil {
		return fmt.Errorf("hash header: %w", err)
	}

	ctx, cancel := context.WithTimeout(ba.ctx, blockRequestTimeout)
	defer cancel()
	network.LogBlockEvent(time.Now(), "requesting", h, header.TimeSlotIndex.ToEpoch(), header.TimeSlotIndex)

	// TODO: GRANDPA, how many blocks back we should even consider.
	// A batch, not one block. One block per announcement is what made the chain
	// stop: the author of a timeslot writes its block at the start of it, and the
	// node that has to build on it wants it at the start of the next one, so a
	// single block per announcement leaves the follower a block behind every time
	// and the author waiting for a block that never arrives. Asking for the stretch
	// behind the announced block fills the gap in one request, which is what the
	// protocol's range is for.
	blocks, err := ba.requestor.RequestBlocks(ctx, h, false, announceBlockBatch, ba.peerKey)
	if err != nil {
		log.Printf("Warning: failed to request block %x: %v", h[:5], err)
		return err
	} else {
		// Process the received blocks
		for _, b := range blocks {
			if err := ba.Store.PutBlock(b); err != nil {
				log.Printf("Warning: failed to store requested block %x: %v", h[:5], err)
				network.LogBlockEvent(time.Now(), "imported", h, b.Header.TimeSlotIndex.ToEpoch(), b.Header.TimeSlotIndex)
			}
			// process custom logic when receiving a new block
			// the hooks are being executed in separate go routines to not slow down the main block receiver
			for _, onBlockReceiveHook := range ba.onBlockReceiveHooks {
				go onBlockReceiveHook(ba.ctx, b)
			}
		}
	}

	return nil
}

// serializeHandshake creates a binary representation of a handshake message
// as defined in the JAMNNP:
// - Finalized block hash (32 bytes)
// - Finalized block slot (4 bytes)
// - Number of leaves (4 bytes)
// - For each leaf: hash (32 bytes) + slot (4 bytes)
func serializeHandshake(finalized chain.LatestFinalized, leaves map[crypto.Hash]jamtime.Timeslot) []byte {
	size := 40 + (len(leaves) * 36)
	content := make([]byte, size)
	offset := 0

	// Add finalized block hash and slot
	copy(content[offset:], finalized.Hash[:])
	offset += 32
	binary.LittleEndian.PutUint32(content[offset:], uint32(finalized.TimeSlotIndex))
	offset += 4

	// Add number of leaves
	binary.LittleEndian.PutUint32(content[offset:], uint32(len(leaves)))
	offset += 4

	// Add each leaf block hash and slot
	for hash, slot := range leaves {
		copy(content[offset:], hash[:])
		offset += 32
		binary.LittleEndian.PutUint32(content[offset:], uint32(slot))
		offset += 4
	}

	return content
}

// headerSlot represents a block hash and its corresponding time slot.
// Used for parsing handshake messages.
type headerSlot struct {
	hash crypto.Hash
	slot jamtime.Timeslot
}

// parseHeaderSlot extracts a header hash and slot from a byte slice.
// Returns an error if the slice is too short.
func parseHeaderSlot(content []byte) (headerSlot, error) {
	if len(content) < 36 {
		return headerSlot{}, fmt.Errorf("content too short for header and a timeslot")
	}
	return headerSlot{
		hash: crypto.Hash(content[:32]),
		slot: jamtime.Timeslot(binary.LittleEndian.Uint32(content[32:36])),
	}, nil
}

// processHandshake processes a received handshake message
// Hash = 32bytes
// Slot = 4 bytes
// len = 4 bytes
// Final = Header Hash ++ Slot
// Leaf = Header Hash ++ Slot
// Handshake = Final ++ len++[Leaf]
func (ba *BlockAnnouncer) processHandshake(content []byte) error {
	if len(content) < 40 { // Hash + Slot + len (without leaves array)
		return fmt.Errorf("handshake message too short")
	}

	// Header + Timeslot of the latest finalized block
	finalized, err := parseHeaderSlot(content[:36])
	if err != nil {
		return fmt.Errorf("parsing finalized header: %w", err)
	}

	// The number of leafs (should be the len of next slice)
	numLeaves := binary.LittleEndian.Uint32(content[36:40])
	expectedSize := 40 + (numLeaves * 36) // 40 until here + 36 for each leaf
	if len(content) != int(expectedSize) {
		return fmt.Errorf("invalid handshake size")
	}

	// Header + Slot of the blocks that have no children
	leaves := make(map[crypto.Hash]jamtime.Timeslot)
	// Starting position of the leaves
	pos := uint32(40)
	for i := uint32(0); i < numLeaves; i++ {
		if pos+36 > uint32(len(content)) {
			return fmt.Errorf("content too short for leaf %d", i)
		}

		leaf, err := parseHeaderSlot(content[pos : pos+36])
		if err != nil {
			return fmt.Errorf("parsing leaf %d: %w", i, err)
		}

		leaves[leaf.hash] = leaf.slot
		pos += 36
	}

	// Store peer's chain state
	ba.mu.Lock()
	defer ba.mu.Unlock()
	ba.peerFinalized = chain.LatestFinalized{
		Hash:          finalized.hash,
		TimeSlotIndex: finalized.slot,
	}
	ba.peerLeaves = leaves
	return nil
}
