package main

import "testing"

// TestParseModelPrefix exercises the model-prefix preprocessor:
// - Recognised model words on top of every start-pattern shape get
//   stripped and returned alongside the rewritten text.
// - Unrecognised words pass through untouched.
// - Continuations and other non-start shapes are still detected by
//   their own parsers after a strip; the dispatch gate that gives
//   strip-meaning lives in HandleAppMention via hasStartPattern.
func TestParseModelPrefix(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantText    string
		wantModel   string
	}{
		{
			"opus + explicit domain",
			"<@U123> opus services: do thing",
			"<@U123> services: do thing",
			"opus",
		},
		{
			"sonnet + auto-name",
			"<@U123> sonnet :: spike",
			"<@U123> :: spike",
			"sonnet",
		},
		{
			"haiku + named template",
			"<@U123> haiku tmpl:: scaffold",
			"<@U123> tmpl:: scaffold",
			"haiku",
		},
		{
			"opus + workspace root",
			"<@U123> opus *: audit",
			"<@U123> *: audit",
			"opus",
		},
		{
			"sonnet + host-direct",
			"<@U123> sonnet !: deploy",
			"<@U123> !: deploy",
			"sonnet",
		},
		{
			"point release",
			"<@U123> claude-opus-4-7 services: thing",
			"<@U123> services: thing",
			"claude-opus-4-7",
		},
		{
			"1m suffix on family name",
			"<@U123> opus[1m] services: thing",
			"<@U123> services: thing",
			"opus[1m]",
		},
		{
			"1m suffix on point release",
			"<@U123> claude-opus-4-6[1m] services: thing",
			"<@U123> services: thing",
			"claude-opus-4-6[1m]",
		},
		{
			"case-insensitive model",
			"<@U123> Opus services: thing",
			"<@U123> services: thing",
			"opus",
		},
		{
			"unknown first word — no strip",
			"<@U123> foobar services: thing",
			"<@U123> foobar services: thing",
			"",
		},
		{
			"domain that happens to be a model name — no whitespace before colon",
			"<@U123> opus: thing",
			"<@U123> opus: thing",
			"",
		},
		{
			"continuation text — no strip",
			"<@U123> opus do the thing",
			"<@U123> do the thing", // strip *would* apply but caller gates on hasStartPattern
			"opus",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotText, gotModel := ParseModelPrefix(c.input)
			if gotText != c.wantText {
				t.Errorf("rewritten text = %q; want %q", gotText, c.wantText)
			}
			if gotModel != c.wantModel {
				t.Errorf("model = %q; want %q", gotModel, c.wantModel)
			}
		})
	}
}

// TestHasStartPattern confirms each start-pattern shape is recognised
// (and that non-start shapes aren't), since the dispatcher in
// HandleAppMention relies on this gate to decide whether to honour a
// stripped model prefix.
func TestHasStartPattern(t *testing.T) {
	yes := []string{
		"<@U123> services: do thing",
		"<@U123> :: do thing",
		"<@U123> tmpl:: do thing",
		"<@U123> *: do thing",
		"<@U123> !: do thing",
	}
	no := []string{
		"<@U123> close",
		"<@U123> set model=opus",
		"<@U123> upload /tmp/file",
		"<@U123> just chatting in a thread",
	}
	for _, s := range yes {
		if !hasStartPattern(s) {
			t.Errorf("expected start pattern: %q", s)
		}
	}
	for _, s := range no {
		if hasStartPattern(s) {
			t.Errorf("did NOT expect start pattern: %q", s)
		}
	}
}
