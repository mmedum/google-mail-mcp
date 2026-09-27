package render

import "github.com/mmedum/google-mail-mcp/internal/model"

// SignatureWrite renders update_signature's result: the signature before
// and after, each in a block, since either may hold text the account's
// owner did not write here.
func SignatureWrite(sw model.SignatureWrite, o Options) Result {
	return render(o, func(w *writer) Result {
		if sw.DryRun {
			w.say("dry run: would set the signature of %s.", setting(sw.Address))
		} else {
			w.say("set the signature of %s.", setting(sw.Address))
		}
		w.say("Gmail adds it when a message is written in Gmail itself; drafts this server writes carry no signature.")
		if sw.Before == "" {
			w.say("signature before: none")
		} else {
			w.settingText("signature before", string(sw.Before))
		}
		if sw.After == "" {
			w.say("signature after: none")
		} else {
			w.settingText("signature after", string(sw.After))
		}
		return Result{Budget: o.budget()}
	})
}

// FilterWrite renders create_filter's and delete_filter's result, in the
// server's voice, the filter shown as list_filters shows it.
func FilterWrite(fw model.FilterWrite) string {
	return plain(func(w *writer) {
		f := fw.Filter
		switch {
		case fw.Op == "create" && fw.DryRun:
			w.say("dry run: would create a filter; no filter does this already.")
		case fw.Op == "create":
			w.say("created filter %s", gmailID(f.ID))
		case fw.DryRun:
			w.say("dry run: would delete filter %s", gmailID(f.ID))
		case fw.Gone:
			w.say("filter %s is gone: Gmail answered the delete that it no longer exists, so it was deleted elsewhere, or by this call on a retry whose first answer was lost.", gmailID(f.ID))
		default:
			w.say("deleted filter %s; mail it already acted on stays as it is.", gmailID(f.ID))
		}
		w.filterBody(f)
		if fw.Op == "create" {
			w.say("It acts on mail that arrives from now on; mail already in the mailbox is untouched.")
		}
	})
}

// VacationWrite renders set_vacation's result: the reply before and
// after, its audience and dates in the server's voice and its text in a
// block, as get_settings shows it.
func VacationWrite(vw model.VacationWrite, o Options) Result {
	return render(o, func(w *writer) Result {
		a := vw.After
		switch {
		case vw.DryRun && a.Enabled:
			w.say("dry run: would turn the vacation reply on · to %s · %s", recipients(a), period(w, a))
		case vw.DryRun:
			w.say("dry run: would turn the vacation reply off; its text is kept.")
		case a.Enabled:
			w.say("vacation reply is ON · to %s · %s", recipients(a), period(w, a))
		default:
			w.say("vacation reply is off; its text is kept.")
		}
		if b := vw.Before; b.Enabled {
			w.say("before: on · to %s · %s", recipients(b), period(w, b))
		} else {
			w.say("before: off")
		}
		w.vacationText(a)
		return Result{Budget: o.budget()}
	})
}
