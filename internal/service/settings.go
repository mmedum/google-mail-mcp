package service

import (
	"context"

	"github.com/mmedum/google-mail-mcp/v2/internal/gmail"
	"github.com/mmedum/google-mail-mcp/v2/internal/model"
)

// Settings reads the account's settings: seven one-unit reads, made at
// once (§7.7). Nothing here changes a setting; changing forwarding is
// written off (§4.1).
func (s *Service) Settings(ctx context.Context) (model.Settings, error) {
	var (
		v    *gmail.VacationSettings
		af   *gmail.AutoForwarding
		fwd  *gmail.ListForwardingAddressesResponse
		imap *gmail.ImapSettings
		pop  *gmail.PopSettings
		lang *gmail.LanguageSettings
		as   *gmail.ListSendAsResponse
	)
	reads := []func(ctx context.Context) error{
		func(ctx context.Context) (err error) { v, err = s.client.Vacation(ctx); return err },
		func(ctx context.Context) (err error) { af, err = s.client.AutoForwarding(ctx); return err },
		func(ctx context.Context) (err error) { fwd, err = s.client.ForwardingAddresses(ctx); return err },
		func(ctx context.Context) (err error) { imap, err = s.client.Imap(ctx); return err },
		func(ctx context.Context) (err error) { pop, err = s.client.Pop(ctx); return err },
		func(ctx context.Context) (err error) { lang, err = s.client.Language(ctx); return err },
		func(ctx context.Context) (err error) { as, err = s.client.SendAs(ctx); return err },
	}
	if _, err := fanOut(ctx, len(reads), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, reads[i](ctx)
	}); err != nil {
		return model.Settings{}, err
	}
	return model.NewSettings(*v, *af, fwd.ForwardingAddresses, *imap, *pop, *lang, as.SendAs), nil
}

// Filters lists the account's filters with their labels named.
func (s *Service) Filters(ctx context.Context) ([]model.Filter, error) {
	ls, res, err := withLabels(ctx, s, s.client.Filters)
	if err != nil {
		return nil, err
	}
	return model.NewFilters(res.Filter, ls.index), nil
}
