// +build !nonostr

package bridgemap

import (
	bnostr "github.com/42wim/matterbridge/bridge/nostr"
)

func init() {
	FullMap["nostr"] = bnostr.New
}
