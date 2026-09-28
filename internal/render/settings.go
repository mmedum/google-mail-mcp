package render

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mmedum/google-mail-mcp/internal/model"
)

// sendAsReserve keeps room after the send-as list for the lines that
// follow it and one more address with its signature.
const sendAsReserve = 3 * maxSettingText

// maxSettingText is the most of one free-text setting — a vacation
// reply, a signature — the rendering shows. The structured result
// carries it whole.
const maxSettingText = 2000

// Settings renders the account's settings, forwarding first: whether
// mail is leaving the account is the reason to read them (§7.7).
func Settings(st model.Settings, o Options) Result {
	return render(o, func(w *writer) Result {
		res := Result{Budget: o.budget()}
		w.say("budget: %s characters", num(res.Budget))
		af := st.AutoForwarding
		if af.Enabled {
			w.say("FORWARDING IS ON: every incoming message is forwarded to %s; the original is then: %s.",
				setting(af.Address), disposition(af.Disposition))
		} else {
			w.say("automatic forwarding: off")
		}
		if len(st.ForwardingAddresses) == 0 {
			w.say("forwarding addresses: none")
		} else {
			w.say("forwarding addresses (where forwarding or a filter may send mail):")
			for _, f := range st.ForwardingAddresses {
				w.say("  %s · %s", setting(f.Address), oneOf(f.VerificationStatus, "accepted", "pending"))
			}
		}

		w.blank()
		v := st.Vacation
		switch {
		case v.Enabled:
			w.say("vacation reply: ON · to %s · %s", recipients(v), period(w, v))
		case v.Subject != "" || v.Body != "":
			w.say("vacation reply: off (one is set up: %s · %s)", recipients(v), period(w, v))
		default:
			w.say("vacation reply: off")
		}
		w.vacationText(v)

		w.blank()
		w.say("send as:")
		for i, s := range st.SendAs {
			// Room for the lines after the list; the rest are named.
			if w.len() > res.Budget-sendAsReserve {
				w.say("  not shown (over the budget): %s; the structured result lists them.",
					plural(len(st.SendAs)-i, "address", "addresses"))
				res.Truncated = true
				break
			}
			flags := fill("")
			if s.Primary {
				flags = fill("%s · primary", flags)
			}
			if s.Default {
				flags = fill("%s · default", flags)
			}
			if s.Alias {
				flags = fill("%s · alias", flags)
			}
			// An empty status is left out rather than shown as "other":
			// Gmail's reference sets one only for non-primary addresses.
			status := fill("")
			if s.VerificationStatus != "" {
				status = fill(" · %s", oneOf(s.VerificationStatus, "accepted", "pending"))
			}
			w.say("  %s · name %s%s%s%s", setting(s.Address), setting(s.DisplayName), status, flags, replyTo(s.ReplyTo))
			if s.Signature != "" {
				w.settingText("signature", string(s.Signature))
			}
		}

		w.blank()
		imap := fill("off")
		if st.Imap.Enabled {
			imap = fill("on, deleting a message in a client: %s", oneOf(st.Imap.ExpungeBehavior,
				"archive", "trash", "deleteForever", "expungeBehaviorUnspecified"))
		}
		w.say("IMAP: %s", imap)
		// Only the two windows Google documents as on are shown as on;
		// anything else, new values included, is not claimed to be.
		pop := fill("off")
		switch st.Pop.AccessWindow {
		case "allMail":
			pop = fill("on for all mail; a message downloaded is then: %s", disposition(st.Pop.Disposition))
		case "fromNowOn":
			pop = fill("on for mail from now on; a message downloaded is then: %s", disposition(st.Pop.Disposition))
		case "", "disabled":
		default:
			pop = fill("unknown (Google answered a value this server does not know)")
		}
		w.say("POP: %s", pop)
		w.say("display language: %s", setting(st.Language))
		return res
	})
}

// settingText writes a free-text setting in a block, cut at
// maxSettingText with a line saying so.
func (w *writer) settingText(what phrase, text string) {
	text = strings.TrimSpace(text)
	total := utf8.RuneCountInString(text)
	if total > maxSettingText {
		text = string([]rune(text)[:maxSettingText])
	}
	w.block(what, "", "", text)
	if total > maxSettingText {
		w.say("cut: %s of %s characters shown; the structured result has the whole text.", num(maxSettingText), num(total))
	}
}

func disposition(d string) part {
	return oneOf(d, "leaveInInbox", "archive", "trash", "markRead", "dispositionUnspecified")
}

func recipients(v model.Vacation) part {
	switch {
	case v.RestrictToContacts && v.RestrictToDomain:
		return fill("contacts in the account's domain only")
	case v.RestrictToContacts:
		return fill("contacts only")
	case v.RestrictToDomain:
		return fill("the account's domain only")
	}
	return fill("every sender")
}

func period(w *writer, v model.Vacation) part {
	switch {
	case !v.Start.IsZero() && !v.End.IsZero():
		return fill("from %s until %s", w.when(v.Start), w.when(v.End))
	case !v.Start.IsZero():
		return fill("from %s, no end", w.when(v.Start))
	case !v.End.IsZero():
		return fill("until %s", w.when(v.End))
	}
	return fill("no dates")
}

func replyTo(addr string) part {
	if addr == "" {
		return fill("")
	}
	return fill(" · replies to %s", setting(addr))
}

// Filters renders the account's filters within the budget. A filter
// that forwards is flagged on its own line: it sends matching mail out
// of the account.
func Filters(fs []model.Filter, o Options) Result {
	return listing(o, fs, "", "filter", "filters",
		func(f model.Filter) string { return f.ID },
		func(w *writer, f model.Filter) {
			w.say("filter %s", gmailID(f.ID))
			w.filterBody(f)
		},
		func(w *writer) {
			w.say("forwarding mail out of the account: %s", plural(model.Forwarding(fs), "filter", "filters"))
		})
}

// filterBody is what a filter matches and does, as list_filters and the
// filter writes show it.
func (w *writer) filterBody(f model.Filter) {
	w.say("  matches: %s", criteria(f))
	if len(f.Add) > 0 {
		w.say("  adds labels: %s", labelList(f.Add))
	}
	// SPAM removed is Gmail's "never send it to Spam", not a label taken
	// off, so it is said as that.
	if remove := slices.DeleteFunc(slices.Clone(f.Remove), func(l model.LabelRef) bool { return l.ID == "SPAM" }); len(remove) > 0 {
		w.say("  removes labels: %s", labelList(remove))
	}
	if f.NeverSpam() {
		w.say("  never sends matching mail to spam")
	}
	if f.Forward != "" {
		w.say("  FORWARDS matching mail to %s", setting(f.Forward))
	}
	if f.Trashes() {
		w.say("  moves matching mail to the trash, where Gmail deletes it after 30 days.")
	}
}

// vacationText is the reply's subject and body in a block, as
// get_settings and set_vacation show it.
func (w *writer) vacationText(v model.Vacation) {
	if v.Subject == "" && v.Body == "" {
		return
	}
	w.settingText("vacation reply", "Subject: "+string(v.Subject)+"\n\n"+string(v.Body))
	if v.BodyFromHTML {
		w.say("note: the vacation reply was converted from HTML; nothing was fetched.")
	}
}

func criteria(f model.Filter) part {
	c := f.Criteria
	out := fill("")
	add := func(p part) {
		if out.s == "" {
			out = p
			return
		}
		out = fill("%s; %s", out, p)
	}
	if c.From != "" {
		add(fill("from %s", setting(c.From)))
	}
	if c.To != "" {
		add(fill("to %s", setting(c.To)))
	}
	if c.Subject != "" {
		add(fill("subject %s", setting(c.Subject)))
	}
	if c.Query != "" {
		add(fill("search %s", setting(c.Query)))
	}
	if c.NegatedQuery != "" {
		add(fill("not %s", setting(c.NegatedQuery)))
	}
	if c.HasAttachment {
		add(fill("has an attachment"))
	}
	if c.ExcludeChats {
		add(fill("not chats"))
	}
	if c.Size > 0 {
		add(fill("size %s %s", oneOf(c.SizeComparison, "larger", "smaller", "unspecified"), size(int64(c.Size))))
	}
	if out.s == "" {
		return fill("everything")
	}
	return out
}
