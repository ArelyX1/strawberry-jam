package authorization

import (
	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/pvm"
	"github.com/eigerco/strawberry/internal/pvm/host_call"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state"
	"github.com/eigerco/strawberry/internal/work"
	"github.com/eigerco/strawberry/pkg/log"
	"github.com/eigerco/strawberry/pkg/serialization/codec/jam"
)

const (
	isAuthorizedCost = 10
)

type AuthPVMInvoker interface {
	InvokePVM(workPackage work.Package, coreIndex uint16) ([]byte, error)
}

type EmptyContext struct{}

type Authorization struct {
	state state.State
}

func New(state state.State) *Authorization {
	return &Authorization{state: state}
}

// InvokePVM ΨI(P, NC) → B ∪ E
func (a *Authorization) InvokePVM(
	workPackage work.Package, // p
	coreCode uint16, // c
) ([]byte, error) {
	// E(p, c)
	args, err := jam.Marshal(struct {
		WorkPackage work.Package
		Core        uint16
	}{
		WorkPackage: workPackage,
		Core:        coreCode,
	})
	if err != nil {
		return nil, err
	}

	encodedCodeWithMeta, err := workPackage.GetAuthorizationCode(a.state.Services)
	if err != nil {
		return nil, err
	}

	var pvmCode service.CodeWithMetadata
	if err := jam.Unmarshal(encodedCodeWithMeta, &pvmCode); err != nil {
		return nil, err
	}

	// The entry point and the ecall indices come from the blob, because the
	// linker decides both. Assuming zero named an offset the current linker
	// does not produce, so an authorization blob would start executing in the
	// middle of whatever the linker laid out first.
	guest, err := pvm.PrepareGuest(pvmCode.Code, "")
	if err != nil {
		return nil, err
	}
	guestCode, err := guest.Code()
	if err != nil {
		return nil, err
	}

	// F ∈ Ω⟨{}⟩∶ (n, ϱ, φ, µ)
	hostCall := func(
		ecall uint64,
		gasCounter pvm.Gas,
		regs pvm.Registers,
		mem pvm.Memory,
		ctx EmptyContext,
	) (pvm.Gas, pvm.Registers, pvm.Memory, EmptyContext, error) {
		// The index the guest emits is assigned by the linker, so it is
		// translated to the canonical id before anything switches on it.
		hostCall, ok := guest.HostCallID(ecall)
		if !ok {
			return gasCounter, regs, mem, ctx, pvm.ErrPanicf("unknown ecall %d", ecall)
		}

		log.VM.Debug().
			Str("host_call", host_call.HostCallName(hostCall)).
			Uint16("core", coreCode).
			Str("phase", "authorization").
			Msg("Host call invoked")

		switch hostCall {
		case uint64(host_call.GasID):
			gasCounter, regs, err = host_call.GasRemaining(gasCounter, regs)
		case uint64(host_call.FetchID):
			gasCounter, regs, mem, err = host_call.Fetch(gasCounter, regs, mem, &workPackage, nil, nil, nil, nil, nil, nil)
		default:
			// (▸, ϱ−10, [φ0,…,φ6, WHAT, φ8,…], µ)
			regs[pvm.R7] = uint64(host_call.WHAT)
			gasCounter -= isAuthorizedCost
		}

		// otherwise if ϱ′ < 0
		if gasCounter < 0 {
			return gasCounter, regs, mem, ctx, pvm.ErrOutOfGas
		}
		return gasCounter, regs, mem, ctx, err
	}

	// (g, r, ∅) = ΨM(pc, 𝒺, GI , E(p, c), F, ∅)
	_, result, _, err := pvm.InvokeWholeProgram(
		guestCode, // pc
		guest.Entry,
		constants.MaxAllocatedGasIsAuthorized,
		args,
		hostCall,
		EmptyContext{},
	)

	return result, err
}
