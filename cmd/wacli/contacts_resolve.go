package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openclaw/wacli/internal/app"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/wa"
	"github.com/spf13/cobra"
	"go.mau.fi/whatsmeow/types"
)

// contactResolution answers "which phone number is this LID?" (and the reverse)
// from the session's verified PN/LID map, so consumers never have to read
// whatsmeow's private tables (#420). An identity without a known pair is
// reported with resolved=false instead of being dropped.
type contactResolution struct {
	Input    string `json:"input"`
	JID      string `json:"jid,omitempty"`
	Phone    string `json:"phone,omitempty"`
	LID      string `json:"lid,omitempty"`
	Name     string `json:"name,omitempty"`
	Resolved bool   `json:"resolved"`
	Error    string `json:"error,omitempty"`
}

func newContactsResolveCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve <lid|phone|jid> [...]",
		Short: "Map LIDs to phone numbers and phone numbers to LIDs",
		Long: "Map each LID to its phone number (and each phone number to its LID) using the\n" +
			"local session's verified mapping. Reads only local state, so it works while\n" +
			"sync --follow is running. Unknown identities are reported with resolved=false.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := withTimeout(context.Background(), flags)
			defer cancel()

			a, lk, err := newReadApp(ctx, flags)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)

			resolver, err := contactReadResolver(ctx, a)
			if err != nil {
				return err
			}
			results := make([]contactResolution, 0, len(args))
			for _, arg := range args {
				if err := ctx.Err(); err != nil {
					return err
				}
				result, err := resolveContactIdentity(ctx, resolver, arg)
				if err != nil {
					return err
				}
				results = append(results, result)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if flags.agent {
				return writeAgentResolutions(flags, results)
			}
			return writeContactResolutions(os.Stdout, flags.asJSON, fullTableOutput(flags.fullOutput), results)
		},
	}
	return cmd
}

func resolveContactIdentity(ctx context.Context, resolver app.ContactResolver, raw string) (contactResolution, error) {
	res := contactResolution{Input: raw}
	if strings.Count(raw, "@") > 1 {
		res.Error = fmt.Sprintf("invalid user JID %q: expected one @ separator", raw)
		return res, nil
	}
	jid, err := wa.ParseUserOrJID(raw)
	if err != nil {
		res.Error = err.Error()
		return res, nil
	}
	jid = jid.ToNonAD()
	if (jid.Server == types.HiddenUserServer || jid.Server == types.DefaultUserServer) && (jid.User == "" || strings.IndexFunc(jid.User, func(r rune) bool { return r < '0' || r > '9' }) >= 0) {
		res.Error = fmt.Sprintf("invalid user JID %q: expected a numeric user", raw)
		return res, nil
	}
	var pn, lid types.JID
	switch jid.Server {
	case types.HiddenUserServer:
		lid = jid
		if resolver != nil {
			mapped, err := resolver.ResolveLIDToPN(ctx, jid)
			if err != nil {
				return contactResolution{}, &localIdentityError{err}
			}
			if mapped.Server == types.DefaultUserServer && mapped.User != "" {
				pn = mapped.ToNonAD()
			}
		}
	case types.DefaultUserServer:
		pn = jid
		if resolver != nil {
			mapped, err := resolver.ResolvePNToLID(ctx, jid)
			if err != nil {
				return contactResolution{}, &localIdentityError{err}
			}
			if mapped.Server == types.HiddenUserServer && mapped.User != "" {
				lid = mapped.ToNonAD()
			}
		}
	default:
		res.Error = fmt.Sprintf("%s is not a user JID; groups and channels have no phone/LID pair", jid)
		return res, nil
	}

	if !lid.IsEmpty() {
		res.LID = lid.String()
	}
	if !pn.IsEmpty() {
		res.JID = pn.String()
		res.Phone = pn.User
	}
	res.Resolved = !pn.IsEmpty() && !lid.IsEmpty()
	if res.Resolved && resolver != nil {
		// Resolvers can fall back to the bare number or JID; neither is a name.
		if name := resolver.ResolveChatName(ctx, pn, ""); name != pn.User && name != pn.String() {
			res.Name = name
		}
	}
	return res, nil
}

func writeContactResolutions(w io.Writer, asJSON, fullOutput bool, results []contactResolution) error {
	if asJSON {
		return out.WriteJSON(w, results)
	}
	tw := newTableWriter(w)
	fmt.Fprintln(tw, "INPUT\tRESOLVED\tPHONE\tLID\tNAME")
	for _, r := range results {
		resolved := "yes"
		if !r.Resolved {
			resolved = "no"
		}
		if r.Error != "" {
			resolved = "error: " + r.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			tableCell(r.Input, 28, fullOutput),
			sanitize(resolved),
			tableCell(r.Phone, 16, fullOutput),
			tableCell(r.LID, 28, fullOutput),
			tableCell(r.Name, 24, fullOutput),
		)
	}
	return tw.Flush()
}
