package grammar

import (
	"strings"
	"testing"
)

// Loading the vendored grammar cleanly is itself the milestone-1 gate.
func TestLoadVendoredGrammar(t *testing.T) {
	g, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, name := range []string{"Program", "TopLevelStatement", "Statement", "Expression", "List", "Parens", "EndOfInput", "%whitespace"} {
		if g.Rule(name) == nil {
			t.Errorf("rule %q missing", name)
		}
	}
	// the keyword lists become ordered-choice rules during assembly
	for _, list := range KeywordLists() {
		rule := g.Rule(list.RuleName)
		if rule == nil {
			t.Errorf("keyword rule %q missing", list.RuleName)
			continue
		}
		words, err := Keywords(list.File)
		if err != nil {
			t.Fatal(err)
		}
		literals := 0
		for _, tok := range rule.Tokens {
			if tok.Type == Literal {
				literals++
			}
		}
		if literals != len(words) {
			t.Errorf("keyword rule %q has %d literals, want %d", list.RuleName, literals, len(words))
		}
	}
}

// Every rule reference must resolve to a defined rule. EndOfInput is
// satisfied by a rule override; %whitespace is character-level and never
// referenced (the tokenizer handles whitespace).
func TestAllReferencesDefined(t *testing.T) {
	g, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range g.RuleNames() {
		rule := g.Rule(name)
		for _, tok := range rule.Tokens {
			if tok.Type != Reference && tok.Type != FunctionCall {
				continue
			}
			if _, isParam := rule.Parameters[tok.Text]; isParam {
				continue
			}
			if g.Rule(tok.Text) == nil {
				t.Errorf("rule %q references undefined rule %q", name, tok.Text)
			}
		}
	}
}

func TestRuleNamesCaseInsensitive(t *testing.T) {
	g, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if g.Rule("program") == nil || g.Rule("PROGRAM") == nil {
		t.Error("rule lookup should be case-insensitive")
	}
}

func TestParseBasics(t *testing.T) {
	g, err := Parse("Rule <- 'A' Other / 'B'\nOther <- 'C'?\n")
	if err != nil {
		t.Fatal(err)
	}
	rule := g.Rule("Rule")
	if rule == nil {
		t.Fatal("Rule missing")
	}
	want := []Token{
		{Literal, "A"},
		{Reference, "Other"},
		{Operator, "/"},
		{Literal, "B"},
	}
	if len(rule.Tokens) != len(want) {
		t.Fatalf("tokens = %v, want %v", rule.Tokens, want)
	}
	for i, w := range want {
		if rule.Tokens[i] != w {
			t.Errorf("token[%d] = %v, want %v", i, rule.Tokens[i], w)
		}
	}
}

func TestParseParameterizedRule(t *testing.T) {
	g, err := Parse("List(D) <- D (',' D)* ','?\n")
	if err != nil {
		t.Fatal(err)
	}
	rule := g.Rule("List")
	if rule == nil {
		t.Fatal("List missing")
	}
	if len(rule.Parameters) != 1 || rule.Parameters["D"] != 0 {
		t.Errorf("parameters = %v", rule.Parameters)
	}
}

func TestParseNestedFunctionCalls(t *testing.T) {
	g, err := Parse("R <- Parens(List(Expression))\n")
	if err != nil {
		t.Fatal(err)
	}
	rule := g.Rule("R")
	want := []Token{
		{FunctionCall, "Parens"},
		{FunctionCall, "List"},
		{Reference, "Expression"},
		{Operator, ")"},
		{Operator, ")"},
	}
	for i, w := range want {
		if rule.Tokens[i] != w {
			t.Errorf("token[%d] = %v, want %v", i, rule.Tokens[i], w)
		}
	}
}

func TestParseComments(t *testing.T) {
	g, err := Parse("# leading comment\nR <- 'A' # trailing comment\nS <- 'B'\n")
	if err != nil {
		t.Fatal(err)
	}
	if g.Rule("R") == nil || g.Rule("S") == nil {
		t.Error("comment handling dropped a rule")
	}
	if len(g.Rule("R").Tokens) != 1 {
		t.Errorf("R tokens = %v", g.Rule("R").Tokens)
	}
}

func TestParsePercentRule(t *testing.T) {
	g, err := Parse("%whitespace <- [ \\t\\n\\r]*\n")
	if err != nil {
		t.Fatal(err)
	}
	rule := g.Rule("%whitespace")
	if rule == nil {
		t.Fatal("%whitespace missing")
	}
	if rule.Tokens[0].Type != Regex {
		t.Errorf("tokens = %v", rule.Tokens)
	}
}

func TestParseNegativeLookahead(t *testing.T) {
	g, err := Parse("R <- !'A' Ident\nIdent <- 'B'\n")
	if err != nil {
		t.Fatal(err)
	}
	rule := g.Rule("R")
	if rule.Tokens[0] != (Token{Operator, "!"}) {
		t.Errorf("negative lookahead not parsed: %v", rule.Tokens)
	}
}

func TestParseMultiLineChoice(t *testing.T) {
	// a trailing '/' continues the rule across the newline
	g, err := Parse("R <- 'A' /\n    'B'\nS <- 'C'\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Rule("R").Tokens) != 3 {
		t.Errorf("R tokens = %v", g.Rule("R").Tokens)
	}
	if g.Rule("S") == nil {
		t.Error("S missing")
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"R <- 'A'\nR <- 'B'\n", "duplicate rule name"},
		{"R <- 'A", "did not find closing"},
		{"R <- 'A'i\n", "unexpected \"i\""},
		{"<- 'A'\n", "expected an alpha-numeric rule name"},
		{"R\n", "does not have a definition"},
		// a stray ')' errors at grammar-parse time; an unbalanced '(' is
		// only caught later, when the matcher factory compiles the rule
		{"R <- 'A')\n", "unclosed bracket"},
	} {
		_, err := Parse(tc.text)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) err = %v, want containing %q", tc.text, err, tc.want)
		}
	}
}

func TestParseSingleRule(t *testing.T) {
	rule, err := ParseSingleRule("R <- 'A' 'B'\n")
	if err != nil {
		t.Fatal(err)
	}
	if rule.Name != "R" || len(rule.Tokens) != 2 {
		t.Errorf("rule = %+v", rule)
	}
	if _, err := ParseSingleRule("R <- 'A'\nS <- 'B'\n"); err == nil {
		t.Error("two rules accepted by ParseSingleRule")
	}
}
