package chain

import (
	"errors"
	"fmt"
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
func NewBlockService(kvStore *pebble.KVStore) (*BlockService, error) {
	chain := store.NewChain(kvStore)
	bs := &BlockService{
		Store:       chain,
		KnownLeaves: make(map[crypto.Hash]jamtime.Timeslot),
		// On by default: it is the behaviour everything else is written against.
		// The devnet turns it off, and only the devnet, because finalizing without
		// anyone having agreed on the block is a decision it has no basis to make.
		finalization: true,
	}
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

	// A dev chain is dated when it is started. JAM's genesis sits at the beginning
	// of JAM time, which is millions of timeslots ago, and a node that started
	// there would have to produce every timeslot since to reach the present.
	genesisHeader := block.Header{
		ParentHash:       GenesisParent,
		TimeSlotIndex:    jamtime.Now().ToTimeslot(),
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

// AddLeaf adds a block to the set of known leaves.
func (bs *BlockService) AddLeaf(hash crypto.Hash, slot jamtime.Timeslot) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.KnownLeaves[hash] = slot
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
