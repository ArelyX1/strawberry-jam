# JAM Service SDK

Everything needed to write a JAM service declaratively, without hand-writing PVM
host calls or state key derivation.

This is a separate Go module so that it can be versioned and consumed on its
own, while a checkout of the repository builds it together with the node through
`go.work`.

## Layout

| Path | What lives there |
| --- | --- |
| `sdk/service.go` | The `Service` contract: `Init`, `Refine`, `Accumulate`, `OnTransfer`, and the `Phase` a handler belongs to. |
| `sdk/context.go` | The host call surface a handler sees: storage reads and writes, preimage lookups, logs, transfers, and the refine report. |
| `sdk/registry.go` | Assigns service ids without renumbering existing services. |
| `sdk/executor.go` | Runs `Init`, `Refine` and `Accumulate` against a cloned state, and commits only when the caller asks. |
| `sdk/scheduler.go` | Decides which services earn coretime. A service with no queued work goes dormant and costs nothing. |
| `sdk/papucoin/` | PAPU, the first real service: supply, balances, nonces, fees, faucet, and EVM relay. |

## Writing a service

A handler is a function over a context. It never touches the state directly, it
reads and writes through the context, and the context enforces the rules:

```go
registry := svc.NewRegistry()

counter := svc.Service{
    Name: "contador",
    Refine: func(ctx svc.RefineContext, item []byte) ([]byte, error) {
        // Must be deterministic: every core in the assigned set runs this
        // and their reports are compared before anything is accumulated.
        return normalise(item)
    },
    Accumulate: func(ctx svc.AccumulateContext, items []svc.RefinedItem) ([]byte, error) {
        previous, _, err := ctx.Read([]byte("total"))
        if err != nil {
            return nil, err
        }
        total := decode(previous)
        for _, refined := range items {
            total = total.Add(total, refined.Number)
        }
        encoded, err := encode(total)
        if err != nil {
            return nil, err
        }
        // Rejected atomically with ErrStorageFull if the resulting footprint
        // costs more than Balance can pay for.
        return nil, ctx.Write([]byte("total"), encoded)
    },
}

id, err := registry.Register(counter)
```

`Init` is the fourth hook and the one genesis uses. It runs once, with no work
items, when the service account is created, which is how an issuer or opening
balances get applied:

```go
papucoin.New(params, issuerAddress, openingBalances, relay)
```

returns a ready `svc.Service` whose `Init` seeds that configuration. The relay
argument is a `RelayVerifier`; `papucoin.NewEVMRelay(chainID)` is the production
one, which recovers the signer of a submitted EVM transaction rather than
trusting a claimed sender. Passing `nil` makes relayed transfers fail closed.


## Storage rules

`Write` follows the PVM semantics rather than a plain map assignment:

- Writing a zero length value deletes the key.
- The write is rejected, atomically, with `ErrStorageFull` when the resulting
  `ThresholdBalance` exceeds the account balance, so a service can never commit
  state it cannot pay for.

Handlers run against a clone, so returning an error leaves the caller's state
untouched.

## Backing onto the node

`Context` mirrors the host calls the PVM exposes. The implementation behind it is
native Go, which is convenient for tests and for a single node, but a native
backend is not consensus. Running a service as a real PVM program, behind a
`CodeHash`, is the path to agreement with other nodes.

## Tests

```
make test          # the node and the SDK
make test-sdk      # only the SDK
```
