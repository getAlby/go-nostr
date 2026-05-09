package nip19

import (
	"github.com/getAlby/go-nostr"
)

func NeventFromRelayEvent(ie nostr.RelayEvent) string {
	v, _ := EncodeEvent(ie.ID, []string{ie.Relay.URL}, ie.PubKey)
	return v
}
