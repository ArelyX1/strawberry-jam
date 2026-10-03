package chain

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/store"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	"github.com/eigerco/strawberry/pkg/network"
)

// BlockService manages the node's view of the blockchain state, including:
// - Known leaf blocks (blocks with no known children)
// - Latest finalized block
// - Block storage and retrieval
//
// It handles block announcements according to UP 0 protocol specification,
// maintaining the set of leaf blocks and tracking finalization status.
type BlockService struct {
	mu              sync.RWMutex
	KnownLeaves     map[crypto.Hash]jamtime.Timeslot // Maps leaf block hashes to their timeslots
	LatestFinalized LatestFinalized                  // Tracks the most recently finalized block
	Store           *store.Chain                     // Persistent block storage
	// pending holds headers that arrived before the blocks they build on. They
	// are the way back out of a gap: the head is the handle to ask for
	// everything that is missing behind it.
	pending map[crypto.Hash]block.Header
	// finalization says whether blocks may be recorded as finalized without
	// anyone having agreed on them. See SetFinalization.
	finalization bool
	// genesisAt is the timeslot a chain not yet on disk is founded at. Zero means
	// the moment the node starts, which only a node on its own can afford.
	genesisAt jamtime.Timeslot
}

// LatestFinalized represents the latest finalized block in the chain.
// A block is considered finalized when it has a chain of 5 descendant blocks
// built on top of it according to the finalization rules.
type LatestFinalized struct {
	Hash          crypto.Hash      // Hash of the finalized block
	TimeSlotIndex jamtime.Timeslot // Timeslot of the finalized block
}

// Leaf represents a block with no known children (a tip of the chain).
// The BlockService tracks all known leaves to implement the UP 0 protocol's
// requirement of announcing all leaves in handshake messages.
type Leaf struct {
	Hash          crypto.Hash      // Hash of the leaf block
	TimeSlotIndex jamtime.Timeslot // Timeslot of the leaf block
}

// NewBlockService initializes a new BlockService with:
// - Empty leaf block set
// - Persistent block storage using PebbleDB
// - Genesis block as the latest finalized block
// NewBlockService opens the chain.
//
// genesisAt is the timeslot a chain that is not on disk yet is founded at. Every
// node has to be given the same one, because the genesis block is the first thing
// two nodes have to agree on: it is the parent of everything, and two nodes that
// founded theirs at different moments are on two different chains from the first
// block and nothing either of them writes will ever be accepted by the other.
// Pass zero to have it dated at the moment the node starts, which is fine for a
// node on its own and wrong for a network.
func NewBlockService(kvStore *pebble.KVStore, genesisAt jamtime.Timeslot) (*BlockService, error) {
	chain := store.NewChain(kvStore)
	bs := &BlockService{
		Store:       chain,
		KnownLeaves: make(map[crypto.Hash]jamtime.Timeslot),
		// On by default: it is the behaviour everything else is written against.
		// The devnet turns it off, and only the devnet, because finalizing without
		// anyone having agreed on the block is a decision it has no basis to make.
		finalization: true,
	}
	bs.genesisAt = genesisAt

	// Initialize by finding leaves and finalized block
	if err := bs.initializeState(); err != nil {
		// Log error but continue - we can recover state as we process blocks
		fmt.Printf("Failed to initialize block manager state: %v\n", err)
	}
	return bs, nil
}

// GenesisParent is what the genesis block names as its parent. It is the marker
// that tells one genesis block from any other, so a node can find the chain it was
// already running, and so the blocks it produced can be told from the block that
// chain began with.
var GenesisParent = crypto.Hash{1}

// initializeState sets up the initial blockchain state:
//  1. Creates and stores the genesis block, unless the chain already has one
//  2. Sets the latest finalized block to the end of the chain it holds
//
// TODO: This is still a `mock` implementation.
func (bs *BlockService) initializeState() error {
	// A chain that already has blocks keeps them. Writing a new genesis over them
	// on every start would found a new chain each time a node was restarted, and
	// would leave the blocks of the old one pointing at a parent nobody has.
	if header, hash, found := bs.GenesisHeader(); found {
		bs.mu.Lock()
		defer bs.mu.Unlock()
		bs.LatestFinalized = LatestFinalized{
			Hash:          hash,
			TimeSlotIndex: header.TimeSlotIndex,
		}
		return nil
	}

	// A dev chain is dated when it is told to be, which is to say the same moment
	// for every node on the network. JAM's genesis sits at the beginning of JAM
	// time, which is millions of timeslots ago, and a node that started there
	// would have to produce every timeslot since to reach the present, so a
	// network passes the timeslot it was founded at instead of taking the moment
	// it happens to be started at. A node on its own has nobody to agree with and
	// dates its own.
	at := bs.genesisAt
	if at == 0 {
		at = jamtime.Now().ToTimeslot()
	}
	genesisHeader := block.Header{
		ParentHash:       GenesisParent,
		TimeSlotIndex:    at,
		BlockAuthorIndex: 0,
	}
	hash, err := genesisHeader.Hash()
	if err != nil {
		return fmt.Errorf("failed to hash genesis block: %w", err)
	}
	if err := bs.Store.PutHeader(genesisHeader); err != nil {
		return fmt.Errorf("failed to store genesis block: %w", err)
	}
	b := block.Block{
		Header: genesisHeader,
	}
	if err := bs.Store.PutBlock(b); err != nil {
		return fmt.Errorf("failed to store genesis block: %w", err)
	}
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.LatestFinalized = LatestFinalized{
		Hash:          hash,
		TimeSlotIndex: genesisHeader.TimeSlotIndex,
	}
	return nil
}

// GenesisHeader returns the genesis block of the chain this store holds, if it has
// one. It is what a node that was restarted recognises its own chain by.
func (bs *BlockService) GenesisHeader() (block.Header, crypto.Hash, bool) {
	var (
		header block.Header
		hash   crypto.Hash
		found  bool
	)
	if _, _, err := bs.Store.FindHeader(func(h block.Header) bool {
		if h.ParentHash != GenesisParent {
			return false
		}
		candidate, err := h.Hash()
		if err != nil {
			return false
		}
		header, hash, found = h, candidate, true
		return true
	}); err != nil {
		return block.Header{}, crypto.Hash{}, false
	}
	return header, hash, found
}

// checkFinalization determines if a block can be finalized by:
// 1. Walking back 5 generations from the given block hash
// 2. If a complete chain of 5 blocks exists, finalizing the oldest block
// 3. Updating the latest finalized pointer
// 4. Removing the finalized block from the leaf set if present
//
// Returns nil if finalization check succeeds, error if any operations fail.
// Note: May return nil even if finalization isn't possible (e.g., missing ancestors).
// This is due to genesis block handling and is not considered an error.
// finalizationEnabled says whether a block this node has never seen anyone else
// agree on may be recorded as finalized.
//
// It is off for the devnet on purpose. Finalizing is a decision about which chain
// is real, and a fixed number of generations is not that decision, it is a guess
// with no agreement behind it. On a chain that forked, the guess picks a branch,
// and the branch can lose: the finalized point is then on a branch nobody is on,
// every later header arrives on the branch that won, walks back to the finalized
// timeslot, finds a different block there, and is rejected as not descending from
// it. The node stops accepting blocks, the chain stops moving, and the only sign
// is that headers are quietly dropped. Turning it off leaves the chain free to
// grow and the nodes free to agree on it, which is the most a devnet with no
// consensus can honestly claim. Quorum finalization is where it comes back.
// SetFinalization turns finalization on or off.
func (bs *BlockService) SetFinalization(on bool) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.finalization = on
}

func (bs *BlockService) checkFinalization(hash crypto.Hash) error {
	if !bs.finalization {
		return nil
	}
	// Start from current header and walk back 6 generations
	currentHash := hash
	var ancestorChain []block.Header

	// Walk back 6`` generations
	for i := 0; i < 6; i++ {
		header, err := bs.Store.GetHeader(currentHash)
		if err != nil {
			if errors.Is(err, store.ErrHeaderNotFound) {
				// If we can't find a parent, we can't finalize
				return nil
			}
			return fmt.Errorf("failed to get header in chain: %w", err)
		}

		ancestorChain = append(ancestorChain, header)
		currentHash = header.ParentHash
	}

	// Get the oldest header (the one we'll finalize)
	finalizeHeader := ancestorChain[len(ancestorChain)-1]
	finalizeHash, err := finalizeHeader.Hash()
	if err != nil {
		return fmt.Errorf("failed to hash header during finalization: %w", err)
	}

	bs.RemoveLeaf(finalizeHash)
	bs.UpdateLatestFinalized(finalizeHash, finalizeHeader.TimeSlotIndex)

	return nil
}

// HandleNewHeader processes a new block header announcement by:
// 1. Storing the header in persistent storage
// 2. Updating the leaf block set (removing parent, adding new block)
// 3. Checking if the parent block can now be finalized
//
// This implements the core block processing logic required by UP 0 protocol,
// maintaining the node's view of chain tips and finalization status.
// StoreImportedBlock stores a block that arrived over the network and keeps the
// leaf set in step with it.
//
// A block used to be written straight into the store and nothing else. The leaf
// set, though, is what the node's tip is read from, and only a header arriving
// over the announcement was ever added to it. So a block whose header arrived
// separately, or whose header was never announced at all, sat in the store with
// the node none the wiser: the tip stayed where it was, the author waiting on
// that exact block concluded it had not arrived and held its timeslot, and the
// walk for gaps started from the old tip and found nothing missing because the
// chain below it was whole.
//
// That is a mesh that stops with every node healthy, every peer connected and
// nothing logged as an error, and it lasts until somebody restarts. Storing the
// block and forgetting to say so is what let it happen.
func (bs *BlockService) StoreImportedBlock(b block.Block) error {
	hash, err := b.Header.Hash()
	if err != nil {
		return fmt.Errorf("hash imported block: %w", err)
	}
	if err := bs.Store.PutBlock(b); err != nil {
		return err
	}
	// The same two steps HandleNewHeader takes, so that a block which arrives
	// without its header still leaves exactly one tip behind it.
	//
	// The parent is removed whether or not its header is here. Checking first
	// looks tidier and is wrong: blocks do not arrive in order, so a stretch of
	// chain coming in newest first finds the parent's header missing every time,
	// skips the removal, and leaves a tip behind on every block of the stretch.
	// Fifty blocks later the node is choosing between fifty tips and calling a
	// timeslot written because one of them names it.
	bs.RemoveLeaf(b.Header.ParentHash)

	// And then on up the chain, for as long as the tips are consecutive. Removing
	// only the block that arrived leaves a tip behind on every block whose child
	// came before it, which is the same pile up one step at a time, and the set of
	// tips is walked on every pass of the sync loop and sorted on every block.
	// The walk stops at the first hash that is not a tip, because past that point
	// the chain belongs to somebody else's history.
	cur := b.Header.ParentHash
	for step := 0; step < leafCollapseSteps; step++ {
		if !bs.RemoveLeafIfPresent(cur) {
			break
		}
		header, err := bs.Store.GetHeader(cur)
		if err != nil {
			break
		}
		cur = header.ParentHash
	}

	bs.AddLeaf(hash, b.Header.TimeSlotIndex)
	return nil
}

func (bs *BlockService) HandleNewHeader(header *block.Header) error {
	// Get the header hash
	hash, err := header.Hash()
	if err != nil {
		return fmt.Errorf("hash header: %w", err)
	}
	// Need to verify this block is a descendant of latest finalized block
	// before considering it as a potential leaf
	isDescendant, err := bs.IsDescendantOfFinalized(header)
	if err != nil {
		// The walk stops when it cannot find a parent. That is a gap, not a
		// reason to throw the header away: the node is behind, and this header
		// is exactly what it needs to go and ask for the blocks it missed.
		if errors.Is(err, store.ErrHeaderNotFound) {
			bs.trackPending(*header, hash)
			return fmt.Errorf("%w: block %s: %v", ErrMissingAncestors, hash, err)
		}
		return fmt.Errorf("check if block is descendant of finalized: %w", err)
	}
	if !isDescendant {
		return fmt.Errorf("block %s is not a descendant of latest finalized block", hash)
	}

	// First store the header
	if err := bs.Store.PutHeader(*header); err != nil {
		return fmt.Errorf("store header: %w", err)
	}

	// Only update leaves if this is a descendant of finalized block
	bs.RemoveLeaf(header.ParentHash)
	bs.AddLeaf(hash, header.TimeSlotIndex)

	// Check if this creates a finalization condition starting from parent
	if err := bs.checkFinalization(header.ParentHash); err != nil {
		// Log but don't fail on finalization check errors
		fmt.Printf("check finalization: %v\n", err)
	}

	return nil
}

// UpdateLatestFinalized updates the latest finalized block pointer.
func (bs *BlockService) UpdateLatestFinalized(hash crypto.Hash, slot jamtime.Timeslot) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.LatestFinalized = LatestFinalized{Hash: hash, TimeSlotIndex: slot}
	network.LogBlockEvent(time.Now(), "finalizing", hash, slot.ToEpoch(), slot)
}

// ResetLeaves empties the set of heads the node knows about.
//
// The set is in memory and is not rebuilt on a restart, so a node that comes back
// has to say where its chain is again. Getting this wrong is not a small thing:
// tip choice and executing a peer's block both start from the leaves, so a stale
// set has the node building on a block the chain left long ago.
func (bs *BlockService) ResetLeaves() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.KnownLeaves = make(map[crypto.Hash]jamtime.Timeslot)
}

// AddLeaf adds a block to the set of known leaves.
func (bs *BlockService) AddLeaf(hash crypto.Hash, slot jamtime.Timeslot) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.KnownLeaves[hash] = slot
	bs.pruneLocked()
}

// leafCap is how many tips the node is willing to keep.
//
// Blocks come back from a peer newest first, so the walk that removes a
// superseded tip finds nothing to remove: the child arrived before the parent and
// became a tip first, and the parent arriving afterwards cannot know it has a
// child. One tip is left behind per block of every stretch imported out of order,
// and the set is walked on every pass of the sync loop and sorted on every
// block, so it has to be bounded rather than merely correct at the end.
//
// What is kept is the newest, because that is the tip the node follows, and the
// old ones are of no use to it: a timeslot is only ever asked for by the tip it
// sits under, and asking about one that is this far back is asking for history.
const leafCap = 64

// pruneLocked drops the oldest tips until the set is back within its bound.
func (bs *BlockService) pruneLocked() {
	if len(bs.KnownLeaves) <= leafCap {
		return
	}
	ordered := make([]crypto.Hash, 0, len(bs.KnownLeaves))
	for hash := range bs.KnownLeaves {
		ordered = append(ordered, hash)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return bs.KnownLeaves[ordered[i]] < bs.KnownLeaves[ordered[j]]
	})
	for _, hash := range ordered[:len(bs.KnownLeaves)-leafCap] {
		delete(bs.KnownLeaves, hash)
	}
}

// GetLatestFinalized safely returns the latest finalized block info.
func (bs *BlockService) GetLatestFinalized() LatestFinalized {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	return bs.LatestFinalized
}

// RemoveLeaf removes a block from the set of known leaves.
func (bs *BlockService) RemoveLeaf(hash crypto.Hash) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	delete(bs.KnownLeaves, hash)
}

// RemoveLeafIfPresent removes a tip and reports whether it was one.
//
// Reporting it is what lets a chain arriving out of order be collapsed onto its
// own end: the walk up from a new block stops at the first hash that is not a
// tip, and that hash is the boundary it has no business crossing.
func (bs *BlockService) RemoveLeafIfPresent(hash crypto.Hash) bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if _, ok := bs.KnownLeaves[hash]; !ok {
		return false
	}
	delete(bs.KnownLeaves, hash)
	return true
}

// HasLeaf checks if a block hash exists in the set of known leaves.
func (bs *BlockService) HasLeaf(hash crypto.Hash) bool {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	_, exists := bs.KnownLeaves[hash]
	return exists
}

// IsDescendantOfFinalized checks if a block is a descendant of the latest finalized block
// by walking back through its ancestors until we either:
// - Find the latest finalized block (true)
// - Find a different block at the same height as latest finalized (false)
// - Can't find a parent (error)
func (bs *BlockService) IsDescendantOfFinalized(header *block.Header) (bool, error) {
	bs.mu.RLock()
	finalizedSlot := bs.LatestFinalized.TimeSlotIndex
	finalizedHash := bs.LatestFinalized.Hash
	bs.mu.RUnlock()

	current := header
	for current.TimeSlotIndex > finalizedSlot {
		parent, err := bs.Store.GetHeader(current.ParentHash)
		if err != nil {
			// The walk stops short of the answer, and the caller has to be able to
			// tell that apart from an answer of no: a header whose ancestors are
			// missing is kept as a handle and the blocks behind it are asked for,
			// which is how a node that has fallen behind walks the chain again.
			return false, fmt.Errorf("get parent block of slot %d (hash %x): %w", current.TimeSlotIndex, current.ParentHash, err)
		}
		current = &parent
	}

	// If we found the finalized block, this is a descendant
	if current.TimeSlotIndex == finalizedSlot {
		currentHash, err := current.Hash()
		if err != nil {
			return false, err
		}
		return currentHash == finalizedHash, nil
	}
	return false, nil
}

// leafCollapseSteps bounds the walk up the chain that removes superseded tips.
// The tips of a chain that is being followed are consecutive, so the walk is a
// handful of steps long; the bound is there so that a cycle or a badly formed
// parent link costs a number instead of a hang.
const leafCollapseSteps = 64
