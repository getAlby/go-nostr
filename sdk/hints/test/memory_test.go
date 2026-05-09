package test

import (
	"testing"

	"github.com/getAlby/go-nostr/sdk/hints/memoryh"
)

func TestMemoryHints(t *testing.T) {
	runTestWith(t, memoryh.NewHintDB())
}
