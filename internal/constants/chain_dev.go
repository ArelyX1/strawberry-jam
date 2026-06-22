//go:build dev

package constants

const (
	NumberOfValidators = 2

	TimeslotsPerEpoch = 12

	TotalNumberOfCores uint16 = 1

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
