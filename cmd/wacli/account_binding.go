package main

import (
	"fmt"
	"path/filepath"

	"github.com/openclaw/wacli/internal/config"
	"github.com/openclaw/wacli/internal/out"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// accountBinding records parser-observed selectors, without retaining argv.
// The first account and a sticky mismatch suffice to detect A/B/A conflicts.
type accountBinding struct {
	name, storeRef  string
	firstAccount    string
	accountSeen     bool
	accountMismatch bool
	storeSeen       bool
	registryCommand bool
	agent           bool // output intent, independent of partially parsed --agent flags
}

// bindingFlagValue preserves pflag's string grammar and metadata. Only Set
// observes selectors: flag-like content/values and arguments after -- do not.
type bindingFlagValue struct {
	pflag.Value
	set func(string) error
}

func (v bindingFlagValue) Set(value string) error {
	if err := v.set(value); err != nil {
		return err
	}
	return v.Value.Set(value)
}

func registerAccountBinding(root *cobra.Command, flags *rootFlags) {
	b := &flags.accountBinding
	b.agent = flags.agent
	fs := root.PersistentFlags()
	account := fs.Lookup("account")
	account.Value = bindingFlagValue{Value: account.Value, set: func(value string) error {
		if b.name != "" && value != b.name {
			return fmt.Errorf("--account conflicts with --for-account")
		}
		if b.accountSeen && value != b.firstAccount {
			b.accountMismatch = true
		}
		if !b.accountSeen {
			b.firstAccount = value
		}
		b.accountSeen = true
		return nil
	}}
	store := fs.Lookup("store")
	store.Value = bindingFlagValue{Value: store.Value, set: func(string) error {
		if b.name != "" {
			return fmt.Errorf("--store cannot be combined with --for-account")
		}
		b.storeSeen = true
		return nil
	}}
	var name string
	fs.StringVarP(&name, "for-account", "a", "", "bind this invocation to an existing account; reject conflicting selectors")
	bound := fs.Lookup("for-account")
	bound.Value = bindingFlagValue{Value: bound.Value, set: func(value string) error {
		if err := config.ValidateAccountName(value); err != nil {
			return err
		}
		if b.registryCommand {
			return fmt.Errorf("--for-account cannot be used with global accounts commands")
		}
		if b.storeSeen {
			return fmt.Errorf("--store cannot be combined with --for-account")
		}
		if (b.name != "" && b.name != value) || b.accountMismatch || (b.accountSeen && b.firstAccount != value) {
			return fmt.Errorf("account selectors conflict with --for-account")
		}
		if b.name != "" {
			return nil
		}
		ref, _, err := config.ResolveAccountStore(config.DefaultConfigPath(), value)
		if err == nil {
			ref, err = filepath.Abs(ref)
		}
		if err != nil {
			selection := agentStoreError(err)
			if b.agent {
				return selection
			}
			// Keep legacy exit/channel behavior without exposing config contents.
			return fmt.Errorf("%s", selection.Message)
		}
		b.name, b.storeRef = value, ref
		if b.agent {
			flags.agentAccount = out.AgentAccount{Name: value, StoreRef: &ref}
		}
		return nil
	}}
}
