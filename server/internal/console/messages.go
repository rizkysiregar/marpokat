package console

import (
	"fmt"
	"html"
)

// The e-mails the website sends. Plain and short on purpose: they are read on a phone, and a link is all they need.

func verifyEmailMessage(to, name, link string) Message {
	return Message{
		To:      to,
		Subject: "Confirm your e-mail for Marpokat",
		Text:    fmt.Sprintf("Hi %s,\n\nConfirm your e-mail address to start creating projects:\n\n%s\n\nThe link works for 24 hours. If you did not sign up, ignore this e-mail.\n", name, link),
		HTML:    fmt.Sprintf(`<p>Hi %s,</p><p>Confirm your e-mail address to start creating projects:</p><p><a href="%s">Confirm e-mail</a></p><p>The link works for 24 hours. If you did not sign up, ignore this e-mail.</p>`, html.EscapeString(name), html.EscapeString(link)),
	}
}

func alreadyRegisteredMessage(to, signInLink, resetLink string) Message {
	return Message{
		To:      to,
		Subject: "You already have a Marpokat account",
		Text:    fmt.Sprintf("Someone tried to sign up to Marpokat with this e-mail address, which already has an account.\n\nSign in: %s\nForgot the password? %s\n\nIf it was not you, ignore this e-mail.\n", signInLink, resetLink),
		HTML:    fmt.Sprintf(`<p>Someone tried to sign up to Marpokat with this e-mail address, which already has an account.</p><p><a href="%s">Sign in</a> · <a href="%s">Forgot the password?</a></p><p>If it was not you, ignore this e-mail.</p>`, html.EscapeString(signInLink), html.EscapeString(resetLink)),
	}
}

func resetPasswordMessage(to, name, link string) Message {
	return Message{
		To:      to,
		Subject: "Reset your Marpokat password",
		Text:    fmt.Sprintf("Hi %s,\n\nSet a new password:\n\n%s\n\nThe link works for 1 hour and only once. If you did not ask for it, ignore this e-mail: your password has not changed.\n", name, link),
		HTML:    fmt.Sprintf(`<p>Hi %s,</p><p>Set a new password:</p><p><a href="%s">Reset password</a></p><p>The link works for 1 hour and only once. If you did not ask for it, ignore this e-mail: your password has not changed.</p>`, html.EscapeString(name), html.EscapeString(link)),
	}
}

func passwordChangedMessage(to, name string) Message {
	return Message{
		To:      to,
		Subject: "Your Marpokat password was changed",
		Text:    fmt.Sprintf("Hi %s,\n\nThe password of your Marpokat account was just changed and every other session was signed out. If it was not you, reset your password at once.\n", name),
		HTML:    fmt.Sprintf(`<p>Hi %s,</p><p>The password of your Marpokat account was just changed and every other session was signed out. If it was not you, reset your password at once.</p>`, html.EscapeString(name)),
	}
}
