package push

import (
	"strconv"
	"unicode/utf8"
)

// MaxTextLength is how much of a message goes into a notification, in characters.
const MaxTextLength = 200

// Content is what a notification says. It becomes the data of the FCM message (spec/events.md, section 11).
type Content struct {
	ChannelID       string
	ChannelType     string
	ChannelName     string
	MessageID       string
	Seq             int64
	SenderID        string // external id
	SenderName      string
	SenderAvatarURL string
	Text            string
	AttachmentKind  string
	UnreadCount     int
}

// Data returns the FCM data: strings only, and a marker so that an app can tell a Marpokat push from its own.
func (c Content) Data() map[string]string {
	return map[string]string{
		"marpokat":          "1",
		"type":              "message.new",
		"channel_id":        c.ChannelID,
		"channel_type":      c.ChannelType,
		"channel_name":      c.ChannelName,
		"message_id":        c.MessageID,
		"seq":               strconv.FormatInt(c.Seq, 10),
		"sender_id":         c.SenderID,
		"sender_name":       c.SenderName,
		"sender_avatar_url": c.SenderAvatarURL,
		"text":              truncateText(c.Text, MaxTextLength),
		"attachment_kind":   c.AttachmentKind,
		"unread_count":      strconv.Itoa(c.UnreadCount),
	}
}

// truncateText cuts at a character, never in the middle of one.
func truncateText(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}
