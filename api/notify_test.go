// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"strings"
	"testing"
)

// The `notify:` block's url+token pair, validated all-or-nothing.
//
// Absent is the only silent case, and it has to stay silent: every
// operator who upgrades this component without asking for
// notifications must keep deploying exactly as before. Everything else
// fails — a publisher that deploys with notifications half-wired is the
// worst available outcome, because the operator believes they are
// covered and the first thing they learn is that nobody was paged.
//
// Sibling of sc-component-backup/api/notify_test.go; keep them
// diffable.
func TestObservatoryConfigValidateNotify(t *testing.T) {
	for _, tc := range []struct {
		name    string
		notify  *Notify
		token   string
		wantErr []string
	}{
		{name: "absent is fine"},
		{
			name:   "url and token",
			notify: &Notify{URL: "https://notify.simple-forge.com"},
			token:  "${secret:sc-notifier-obs-event-token}",
		},
		{
			name:   "operator-written resource reference and token",
			notify: &Notify{Name: "my-notifier", URL: "$sc:${resource:my-notifier.self-url}"},
			token:  "${secret:sc-notifier-obs-event-token}",
		},
		{
			name:    "url without a token",
			notify:  &Notify{URL: "https://notify.simple-forge.com"},
			wantErr: []string{"notifyToken", "401", "sc secrets add"},
		},
		{
			name:    "token without a notify block",
			token:   "${secret:sc-notifier-obs-event-token}",
			wantErr: []string{"notifyToken", "no `notify:` block"},
		},
		{
			name:    "notify block with no url",
			notify:  &Notify{},
			token:   "${secret:t}",
			wantErr: []string{"no `url`", "remove the `notify:` block"},
		},
		{
			// A name alone cannot resolve a URL: a plugin cannot read a
			// sibling resource's declaration, so the message hands over
			// the line the operator writes instead.
			name:    "notify names a resource but gives no url",
			notify:  &Notify{Name: "my-notifier"},
			token:   "${secret:t}",
			wantErr: []string{"names a resource but no URL", "my-notifier.self-url"},
		},
		{
			name:    "whitespace-only token does not count",
			notify:  &Notify{URL: "https://notify.simple-forge.com"},
			token:   "   ",
			wantErr: []string{"notifyToken", "401"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ObservatoryConfig{Notify: tc.notify, NotifyToken: tc.token}
			err := c.validateNotify()

			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message does not mention %q:\n  %v", want, err)
				}
			}
		})
	}
}

// The pair check has to be reachable from the top-level Validate, not
// just from the helper — otherwise it never runs on a real config.
func TestObservatoryConfigValidateReachesTheNotifyCheck(t *testing.T) {
	c := &ObservatoryConfig{
		DeployTo:   "lambda",
		Image:      "simplecontainer/sc-component-observatory:v1",
		SigningKey: "${secret:k}",
		Notify:     &Notify{URL: "https://notify.simple-forge.com"},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("a notify block with no token must fail the whole config")
	}
	if !strings.Contains(err.Error(), "notifyToken") {
		t.Errorf("message does not name the missing key:\n  %v", err)
	}

	c.NotifyToken = "${secret:sc-notifier-obs-event-token}"
	if err := c.Validate(); err != nil {
		t.Fatalf("the complete pair must validate: %v", err)
	}
}
