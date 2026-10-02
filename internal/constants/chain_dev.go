//go:build dev

package constants

const (
	// NumberOfValidators is a parameter of the chain, like the length of a
	// timeslot, and it is fixed when the chain is defined: two nodes that
	// disagreed about it would be on different chains, so it cannot be something
	// each process decides for itself. It is a ceiling for the dev chain, and the
	// validator file says how many of them a given run actually uses. It is six
	// rather than two so that a mesh of more than two nodes is possible at all,
	// which it was not: the validator set is a fixed-size array, so a third
	// validator was an index out of range.
	NumberOfValidators = 32

	TimeslotsPerEpoch = 12

	// Two cores, not one: several state-transition and host-call tests index
	// core 1 into fixed-size [TotalNumberOfCores]T arrays, and JAM's dispute and
	// authorization tests need a second core to observe re-assignment.
	TotalNumberOfCores uint16 = 2

	MaxTimeslotsForLookupAnchor = 14400

	ValidatorRotationPeriod = 4

	PreimageExpulsionPeriod = 32

	TicketSubmissionTimeSlots = 10

	MaxTicketExtrinsicSize = 3

	MaxTicketAttemptsPerValidator = 3

	MaxTicketsPerBlock = 3

	NumberOfErasureCodecPiecesInSegment = 1026

	ErasureCodingOriginalShards = 2

	ErasureCodingChunkSize = 4

	MaxAllocatedGasRefine = 1_000_000_000

	TotalGasAccumulation = 20_000_000
)
