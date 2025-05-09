package bnostr

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/42wim/matterbridge/bridge"
	"github.com/42wim/matterbridge/bridge/config"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

type Bnostr struct {
	defaultRelayUri string
	rooms           map[string]*Broom // Using map for O(1) lookup
	nicks           map[string]string // map from npub to nick
	publicKeyHex    string
	privateKeyHex   string
	*bridge.Config
}

type Broom struct {
	addr  string
	naddr string
	relay *nostr.Relay
	ctx   context.Context
	stop  context.CancelFunc
}

func New(cfg *bridge.Config) bridge.Bridger {
	b := &Bnostr{
		Config: cfg,
		rooms:  make(map[string]*Broom),
		nicks:  make(map[string]string),
	}

	// Get and validate required config values
	defaultRelayUri := b.GetString("DefaultRelay")
	if defaultRelayUri == "" {
		b.Log.Panic("Missing DefaultRelay configuration")
	}
	b.defaultRelayUri = defaultRelayUri

	pubKey := b.GetString("PublicKey")
	if pubKey == "" {
		b.Log.Panic("Missing PublicKey configuration")
	}

	privKey := b.GetString("PrivateKey")
	if privKey == "" {
		b.Log.Panic("Missing PrivateKey configuration")
	}

	// Decode and validate keys
	if prefix, pubHex, err := nip19.Decode(pubKey); err != nil || prefix != "npub" {
		b.Log.Panicf("Invalid public key format: %v", err)
	} else {
		b.publicKeyHex = pubHex.(string)
	}

	if prefix, secHex, err := nip19.Decode(privKey); err != nil || prefix != "nsec" {
		b.Log.Panicf("Invalid private key format: %v", err)
	} else {
		b.privateKeyHex = secHex.(string)
	}

	b.Log.Debugf("Initialized with relay: %s, pubkey: %s", b.defaultRelayUri, b.publicKeyHex)
	return b
}

func (b *Bnostr) Connect() error { return nil }

func (b *Bnostr) Disconnect() error {
	for _, room := range b.rooms {
		room.stop()
	}
	return nil
}

func (b *Bnostr) JoinChannel(channel config.ChannelInfo) error {
	// Decode the nostr address
	prefix, decoded, err := nip19.Decode(channel.Name)
	if err != nil {
		return fmt.Errorf("failed to decode naddr: %v", err)
	}
	b.Log.Debugf("Decoded channel %s: %v %v", channel.Name, prefix, decoded)

	ep, ok := decoded.(nostr.EntityPointer)
	if !ok {
		return fmt.Errorf("invalid entity pointer: %v", decoded)
	}

	addr := fmt.Sprintf("%v:%v:%v", ep.Kind, ep.PublicKey, ep.Identifier)
	relayUrl := ep.Relays[0]

	if relayUrl == "" {
		relayUrl = b.defaultRelayUri
	}

	ctx, cancel := context.WithCancel(context.Background())
	relay, err := nostr.RelayConnect(ctx, relayUrl)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to connect to relay %s: %v", relayUrl, err)
	}

	room := &Broom{
		addr:  addr,
		naddr: channel.Name,
		relay: relay,
		ctx:   ctx,
		stop:  cancel,
	}

	b.rooms[channel.Name] = room
	go b.streamMessages(room)

	b.Log.Infof("Joined channel %s at relay %s", addr, relayUrl)
	return nil
}

func (b *Bnostr) streamMessages(room *Broom) {
	timestamp := nostr.Timestamp(time.Now().Unix())
	filters := nostr.Filters{{
		Kinds: []int{nostr.KindLiveChatMessage},
		Tags:  nostr.TagMap{"a": {room.addr}},
		Since: &timestamp,
	}}

	sub, err := room.relay.Subscribe(room.ctx, filters)
	if err != nil {
		b.Log.Errorf("Failed to subscribe to %s: %v", room.addr, err)
		return
	}
	defer sub.Close()

	for ev := range sub.Events {
		if ev.PubKey == b.publicKeyHex {
			continue // Skip own messages
		}

		b.Remote <- config.Message{
			Text:     ev.Content,
			Channel:  room.naddr,
			Username: b.getNick(room, ev.PubKey),
			UserID:   ev.PubKey,
			ID:       ev.ID,
			Account:  b.Account,
		}
	}
}

type ProfileMetadata struct {
	Name string `json:"name"`
}

func (b *Bnostr) getNick(room *Broom, npub string) string {
	nick, ok := b.nicks[npub]
	if ok {
		return nick
	}

	filters := nostr.Filters{{
		Kinds:   []int{nostr.KindProfileMetadata},
		Authors: []string{npub},
		Limit:   1,
	}}

	b.Log.Debugf("Looking up nick: %s", npub)

	sub, err := room.relay.Subscribe(room.ctx, filters)
	if err != nil {
		b.Log.Errorf("Failed to subscribe to %s: %v", npub, err)
		return "unknown"
	}
	defer sub.Close()

	for ev := range sub.Events {
		var pm ProfileMetadata
		json.Unmarshal([]byte(ev.Content), &pm)
		b.nicks[npub] = pm.Name
		return pm.Name
	}

	return "unknown"
}

func (b *Bnostr) Send(msg config.Message) (string, error) {
	room, exists := b.rooms[msg.Channel]
	if !exists {
		return "", fmt.Errorf("room not found: %s", msg.Channel)
	}

	if msg.Event == config.EventMsgDelete {
		b.Log.Warn("Message deletion not supported for Nostr")
		return "", nil
	}

	if msg.Event == "" {
		event := nostr.Event{
			PubKey:    b.publicKeyHex,
			CreatedAt: nostr.Now(),
			Kind:      nostr.KindLiveChatMessage,
			Tags:      nostr.Tags{{"a", room.addr}},
			Content:   msg.Username + msg.Text,
		}
		fmt.Println("Signing event:", event)
		if err := event.Sign(b.privateKeyHex); err != nil {
			return "", fmt.Errorf("failed to sign event: %v", err)
		}
		fmt.Println("Publishing event:", event)
		if err := room.relay.Publish(room.ctx, event); err != nil {
			return "", fmt.Errorf("failed to publish event: %v", err)
		}

		return event.ID, nil
	}

	return "", nil
}
