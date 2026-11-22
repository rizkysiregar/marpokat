// Package outbox writes events in the same transaction as the change they describe (PLAN D6). The worker relay
// (M4) publishes them to RabbitMQ afterwards, so an event exists if and only if its change was committed.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// Topics. Payloads carry ids only; consumers load what they need.
const (
	TopicMessageCreated      = "message.created"       // {channel_id, message_id, seq, sender_id, type}
	TopicChannelCreated      = "channel.created"       // {channel_id, type, member_ids}
	TopicChannelUpdated      = "channel.updated"       // {channel_id}
	TopicChannelMembersAdded = "channel.members_added" // {channel_id, user_ids}
	TopicChannelMemberLeft   = "channel.member_left"   // {channel_id, user_id, reason}
)

// Enqueue adds an event to the outbox using q, which must be the transaction of the change.
func Enqueue(ctx context.Context, q store.Querier, tenantID uuid.UUID, topic string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal %s: %w", topic, err)
	}
	if err := q.InsertOutbox(ctx, store.InsertOutboxParams{TenantID: tenantID, Topic: topic, Payload: string(b)}); err != nil {
		return fmt.Errorf("outbox: insert %s: %w", topic, err)
	}
	return nil
}
