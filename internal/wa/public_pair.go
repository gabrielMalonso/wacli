package wa

import (
	"context"
	"fmt"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

type PublicPairResult uint8

const (
	PublicPairUnverified PublicPairResult = iota
	PublicPairVerified
	PublicPairContradictory
)

// CheckPublicPair corroborates an asserted PN/LID pair using only this client's
// local public map. It neither discovers an alias nor promises an atomic snapshot
// of the two reads. Storage failures remain errors, never missing mappings.
func (c *Client) CheckPublicPair(ctx context.Context, first, second types.JID) (PublicPairResult, error) {
	c.mu.Lock()
	cli := c.client
	c.mu.Unlock()
	if cli == nil || cli.Store == nil || cli.Store.LIDs == nil {
		return PublicPairUnverified, fmt.Errorf("public identity map unavailable")
	}
	return checkPublicPair(ctx, cli.Store.LIDs, first, second)
}

func checkPublicPair(ctx context.Context, mapping store.LIDStore, first, second types.JID) (PublicPairResult, error) {
	pn, lid := first.ToNonAD(), second.ToNonAD()
	if pn.Server == types.HiddenUserServer {
		pn, lid = lid, pn
	}
	if pn.Server != types.DefaultUserServer || lid.Server != types.HiddenUserServer || pn.User == "" || lid.User == "" || pn.Integrator != 0 || lid.Integrator != 0 {
		return PublicPairUnverified, fmt.Errorf("invalid public identity pair")
	}
	actualLID, err := mapping.GetLIDForPN(ctx, pn)
	if err != nil {
		return PublicPairUnverified, err
	}
	actualPN, err := mapping.GetPNForLID(ctx, lid)
	if err != nil {
		return PublicPairUnverified, err
	}
	if !actualLID.IsEmpty() && actualLID.ToNonAD() != lid || !actualPN.IsEmpty() && actualPN.ToNonAD() != pn {
		return PublicPairContradictory, nil
	}
	if actualLID.IsEmpty() || actualPN.IsEmpty() {
		return PublicPairUnverified, nil
	}
	return PublicPairVerified, nil
}
