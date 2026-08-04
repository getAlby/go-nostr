// The sdk package depends on github.com/fiatjaf/eventstore, which is built
// against upstream github.com/nbd-wtf/go-nostr and is therefore not
// type-compatible with this fork's renamed module. This nested go.mod
// carves the directory out of the root module so `go build ./...` and
// `go test ./...` skip it.
module github.com/getAlby/go-nostr/sdk

go 1.24.1
