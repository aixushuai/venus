package vmcontext

import (
	"context"
	"fmt"
	"github.com/filecoin-project/go-address"
	e "github.com/filecoin-project/go-filecoin/internal/pkg/enccid"
	vmErrors "github.com/filecoin-project/go-filecoin/internal/pkg/vm/internal/errors"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm/internal/gascost"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm/internal/pattern"
	"github.com/filecoin-project/go-filecoin/internal/pkg/vm/internal/runtime"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/filecoin-project/go-state-types/crypto"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/filecoin-project/go-state-types/network"
	"github.com/filecoin-project/go-state-types/rt"
	rtt "github.com/filecoin-project/go-state-types/rt"
	specsruntime "github.com/filecoin-project/specs-actors/actors/runtime"
	"github.com/ipfs/go-cid"
	cbor2 "github.com/ipfs/go-ipld-cbor"
	logging "github.com/ipfs/go-log/v2"
)

var EmptyObjectCid cid.Cid

func init() {
	cst := cbor2.NewMemCborStore()
	emptyobject, err := cst.Put(context.TODO(), []struct{}{})
	if err != nil {
		panic(err)
	}

	EmptyObjectCid = emptyobject
}

var actorLog = logging.Logger("actors")

type runtimeAdapter struct {
	ctx *invocationContext
	syscalls
}

func newRuntimeAdapter(ctx *invocationContext) *runtimeAdapter {
	return &runtimeAdapter{ctx: ctx, syscalls: syscalls{
		impl:      ctx.rt.syscalls,
		ctx:       ctx.rt.context,
		gasTank:   ctx.gasTank,
		pricelist: ctx.rt.pricelist,
		head:      ctx.rt.currentHead,
		state:     ctx.rt.stateView(),
	}}
}

func (a *runtimeAdapter) Caller() address.Address {
	return a.ctx.Message().Caller()
}

func (a *runtimeAdapter) Receiver() address.Address {
	return a.ctx.Message().Receiver()
}

func (a *runtimeAdapter) ValueReceived() abi.TokenAmount {
	return a.ctx.Message().ValueReceived()
}

func (a *runtimeAdapter) StateCreate(obj cbor.Marshaler) {
	c := a.StorePut(obj)
	err := a.stateCommit(EmptyObjectCid, c)
	if err != nil {
		panic(fmt.Errorf("failed to commit state after creating object: %w", err))
	}
}

func (a *runtimeAdapter) stateCommit(oldh, newh cid.Cid) vmErrors.ActorError {

	// TODO: we can make this more efficient in the future...
	act, found, err := a.ctx.rt.state.GetActor(a.Context(), a.Receiver())
	if !found || err != nil {
		return vmErrors.Escalate(err, "failed to get actor to commit state")
	}

	if act.Head.Cid != oldh {
		return vmErrors.Fatal("failed to update, inconsistent base reference")
	}

	act.Head = e.NewCid(newh)

	if err := a.ctx.rt.state.SetActor(a.Context(), a.Receiver(), act); err != nil {
		return vmErrors.Fatalf("failed to set actor in commit state: %s", err)
	}

	return nil
}

func (a *runtimeAdapter) StateReadonly(obj cbor.Unmarshaler) {
	act, found, err := a.ctx.rt.state.GetActor(a.Context(), a.Receiver())
	if !found || err != nil {
		a.Abortf(exitcode.SysErrorIllegalArgument, "failed to get actor for Readonly state: %s", err)
	}
	a.StoreGet(act.Head.Cid, obj)
}

func (a *runtimeAdapter) StateTransaction(obj cbor.Er, f func()) {
	if obj == nil {
		a.Abortf(exitcode.SysErrorIllegalActor, "Must not pass nil to Transaction()")
	}

	act, found, err := a.ctx.rt.state.GetActor(a.Context(), a.Receiver())
	if !found || err != nil {
		a.Abortf(exitcode.SysErrorIllegalActor, "failed to get actor for Transaction: %s", err)
	}
	baseState := act.Head
	a.StoreGet(baseState.Cid, obj)

	a.ctx.allowSideEffects = false
	f()
	a.ctx.allowSideEffects = true

	c := a.StorePut(obj)

	err = a.stateCommit(baseState.Cid, c)
	if err != nil {
		panic(fmt.Errorf("failed to commit state after transaction: %w", err))
	}
}

func (a *runtimeAdapter) StoreGet(c cid.Cid, o cbor.Unmarshaler) bool {
	return a.ctx.Store().StoreGet(c, o)
}

func (a *runtimeAdapter) StorePut(x cbor.Marshaler) cid.Cid {
	return a.ctx.Store().StorePut(x)
}

func (a *runtimeAdapter) NetworkVersion() network.Version {
	return a.state.GetNtwkVersion(a.Context(), a.CurrEpoch())
}

func (a *runtimeAdapter) GetRandomnessFromBeacon(personalization crypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte) abi.Randomness {
	res, err := a.ctx.randSource.GetRandomnessFromBeacon(a.Context(), personalization, randEpoch, entropy)
	if err != nil {
		panic(vmErrors.Fatalf("could not get randomness: %s", err))
	}
	return res
}

func (a *runtimeAdapter) GetRandomnessFromTickets(personalization crypto.DomainSeparationTag, randEpoch abi.ChainEpoch, entropy []byte) abi.Randomness {
	res, err := a.ctx.randSource.Randomness(a.Context(), personalization, randEpoch, entropy)
	if err != nil {
		panic(vmErrors.Fatalf("could not get randomness: %s", err))
	}
	return res
}

func (a *runtimeAdapter) Send(toAddr address.Address, methodNum abi.MethodNum, params cbor.Marshaler, value abi.TokenAmount, out cbor.Er) exitcode.ExitCode {
	return a.ctx.Send(toAddr, methodNum, params, value, out)
}

func (a *runtimeAdapter) ChargeGas(name string, compute int64, virtual int64) {
	err := a.gasTank.Charge(gascost.NewGasCharge(name, compute, 0).WithVirtual(virtual, 0), "runtimeAdapter charge gas")
	if err != nil {
		panic(err)
	}
}

func (a *runtimeAdapter) Log(level rt.LogLevel, msg string, args ...interface{}) {
	switch level {
	case rtt.DEBUG:
		actorLog.Debugf(msg, args...)
	case rtt.INFO:
		actorLog.Infof(msg, args...)
	case rtt.WARN:
		actorLog.Warnf(msg, args...)
	case rtt.ERROR:
		actorLog.Errorf(msg, args...)
	}
}

var _ specsruntime.Runtime = (*runtimeAdapter)(nil)

// Message implements Runtime.
func (a *runtimeAdapter) Message() specsruntime.Message {
	return a.ctx.Message()
}

// CurrEpoch implements Runtime.
func (a *runtimeAdapter) CurrEpoch() abi.ChainEpoch {
	return a.ctx.Runtime().CurrentEpoch()
}

// ImmediateCaller implements Runtime.
func (a *runtimeAdapter) ImmediateCaller() address.Address {
	return a.ctx.Message().Caller()
}

// ValidateImmediateCallerAcceptAny implements Runtime.
func (a *runtimeAdapter) ValidateImmediateCallerAcceptAny() {
	a.ctx.ValidateCaller(pattern.Any{})
}

// ValidateImmediateCallerIs implements Runtime.
func (a *runtimeAdapter) ValidateImmediateCallerIs(addrs ...address.Address) {
	a.ctx.ValidateCaller(pattern.AddressIn{Addresses: addrs})
}

// ValidateImmediateCallerType implements Runtime.
func (a *runtimeAdapter) ValidateImmediateCallerType(codes ...cid.Cid) {
	a.ctx.ValidateCaller(pattern.CodeIn{Codes: codes})
}

// CurrentBalance implements Runtime.
func (a *runtimeAdapter) CurrentBalance() abi.TokenAmount {
	return a.ctx.Balance()
}

// ResolveAddress implements Runtime.
func (a *runtimeAdapter) ResolveAddress(addr address.Address) (address.Address, bool) {
	return a.ctx.rt.normalizeAddress(addr)
}

// GetActorCodeCID implements Runtime.
func (a *runtimeAdapter) GetActorCodeCID(addr address.Address) (ret cid.Cid, ok bool) {
	entry, found, err := a.ctx.rt.state.GetActor(a.Context(), addr)
	if err != nil {
		panic(vmErrors.Fatalf("failed to get actor: %s", err))
	}

	if !found {
		return cid.Undef, false
	}

	return entry.Code.Cid, true
}

// Abortf implements Runtime.
func (a *runtimeAdapter) Abortf(errExitCode exitcode.ExitCode, msg string, args ...interface{}) {
	runtime.Abortf(errExitCode, msg, args...)
}

// NewActorAddress implements Runtime.
func (a *runtimeAdapter) NewActorAddress() address.Address {
	return a.ctx.NewActorAddress()
}

// CreateActor implements Runtime.
func (a *runtimeAdapter) CreateActor(codeID cid.Cid, addr address.Address) {
	a.ctx.CreateActor(codeID, addr)
}

// DeleteActor implements Runtime.
func (a *runtimeAdapter) DeleteActor(beneficiary address.Address) {
	a.ctx.DeleteActor(beneficiary)
}

func (a *runtimeAdapter) TotalFilCircSupply() abi.TokenAmount {
	return a.state.TotalFilCircSupply(a.CurrEpoch(), a.ctx.rt.state)
}

// Context implements Runtime.
// Dragons: this can disappear once we have the storage abstraction
func (a *runtimeAdapter) Context() context.Context {
	return a.ctx.rt.context
}

var nullTraceSpan = func() {}

// StartSpan implements Runtime.
func (a *runtimeAdapter) StartSpan(name string) func() {
	// Dragons: leeave empty for now, add TODO to add this into gfc
	return nullTraceSpan
}

func (a *runtimeAdapter) AbortStateMsg(msg string) {
	panic(vmErrors.NewfSkip(3, 101, msg))
}
