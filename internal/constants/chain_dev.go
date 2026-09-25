//go:build dev

package constants

const (
	NumberOfValidators = 2

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
