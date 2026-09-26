package gapi

// unitCost is Google's quota cost per method, in units, from the Gmail
// quota reference (developers.google.com/workspace/gmail/api/reference/quota,
// read 2026-09-25). The budget is 6,000 units per user per minute, and
// costs differ by 100×, so a request-rate limiter would be wrong by that
// much (§4.9).
//
// Keyed by the discovery method id a Call carries. A Call whose id is
// missing here is refused before it is sent: an unpriced call would
// spend quota the budget cannot see.
//
// The quota page prices neither labels.patch nor settings.getLanguage.
// They take the price of their nearest listed sibling — labels.update
// and the other settings reads — until a live run says otherwise.
var unitCost = map[string]int{
	"gmail.users.drafts.create":                     10,
	"gmail.users.drafts.delete":                     10,
	"gmail.users.drafts.get":                        20,
	"gmail.users.drafts.list":                       5,
	"gmail.users.drafts.send":                       100,
	"gmail.users.drafts.update":                     15,
	"gmail.users.getProfile":                        1,
	"gmail.users.history.list":                      2,
	"gmail.users.labels.create":                     5,
	"gmail.users.labels.delete":                     5,
	"gmail.users.labels.get":                        1,
	"gmail.users.labels.list":                       1,
	"gmail.users.labels.patch":                      5,
	"gmail.users.labels.update":                     5,
	"gmail.users.messages.attachments.get":          20,
	"gmail.users.messages.batchDelete":              50,
	"gmail.users.messages.batchModify":              50,
	"gmail.users.messages.delete":                   10,
	"gmail.users.messages.get":                      20,
	"gmail.users.messages.import":                   25,
	"gmail.users.messages.insert":                   25,
	"gmail.users.messages.list":                     5,
	"gmail.users.messages.modify":                   5,
	"gmail.users.messages.send":                     100,
	"gmail.users.messages.trash":                    20,
	"gmail.users.messages.untrash":                  5,
	"gmail.users.settings.filters.get":              1,
	"gmail.users.settings.filters.list":             1,
	"gmail.users.settings.forwardingAddresses.get":  1,
	"gmail.users.settings.forwardingAddresses.list": 1,
	"gmail.users.settings.getAutoForwarding":        1,
	"gmail.users.settings.getImap":                  1,
	"gmail.users.settings.getLanguage":              1,
	"gmail.users.settings.getPop":                   1,
	"gmail.users.settings.getVacation":              1,
	"gmail.users.settings.sendAs.get":               1,
	"gmail.users.settings.sendAs.list":              1,
	"gmail.users.threads.delete":                    20,
	"gmail.users.threads.get":                       40,
	"gmail.users.threads.list":                      10,
	"gmail.users.threads.modify":                    10,
	"gmail.users.threads.trash":                     20,
	"gmail.users.threads.untrash":                   10,
}

// Units returns the quota cost of a method, and whether it is priced.
func Units(id string) (int, bool) {
	n, ok := unitCost[id]
	return n, ok
}

// maxUnitCost is the dearest single call, which the budget's burst must
// be able to admit.
func maxUnitCost() int {
	most := 0
	for _, n := range unitCost {
		most = max(most, n)
	}
	return most
}

// sendIDs are the two methods that deliver mail. Neither is ever
// retried, for any status: a second attempt whose first landed is a
// second delivery to every recipient (§4.3).
var sendIDs = map[string]bool{
	"gmail.users.drafts.send":   true,
	"gmail.users.messages.send": true,
}
