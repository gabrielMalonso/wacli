package wa

import (
	"context"
	"errors"
	"testing"

	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

type publicPairErrorMap struct {
	wmstore.LIDStore
	err error
}

func (m publicPairErrorMap) GetLIDForPN(context.Context, types.JID) (types.JID, error) {
	return types.JID{}, m.err
}

func TestCheckPublicPairLocalOnlyErrorsAndContradictions(t *testing.T) {
	c := outboundSDKFixture(t) // no HTTP allowed; never connects
	pn := types.NewJID("15550000002", types.DefaultUserServer)
	lid := types.NewJID("90002", types.HiddenUserServer)
	if got, err := c.CheckPublicPair(t.Context(), pn, lid); err != nil || got != PublicPairUnverified {
		t.Fatal(got, err)
	}
	if err := c.client.Store.LIDs.PutLIDMapping(t.Context(), lid, pn); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]types.JID{{pn, lid}, {lid, pn}} {
		if got, err := c.CheckPublicPair(t.Context(), pair[0], pair[1]); err != nil || got != PublicPairVerified {
			t.Fatal(got, err)
		}
	}
	other := types.NewJID("90003", types.HiddenUserServer)
	if got, err := c.CheckPublicPair(t.Context(), pn, other); err != nil || got != PublicPairContradictory {
		t.Fatal(got, err)
	}
	failure := errors.New("synthetic SQL failure")
	c.client.Store.LIDs = publicPairErrorMap{err: failure}
	if got, err := c.CheckPublicPair(t.Context(), pn, lid); !errors.Is(err, failure) || got != PublicPairUnverified {
		t.Fatal("SQL failure treated as absence", got, err)
	}
	if _, err := c.CheckPublicPair(t.Context(), pn, pn); err == nil {
		t.Fatal("same namespace accepted")
	}
}
