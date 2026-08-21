package grammar

import (
	"fmt"
	"strings"
)

// TokenType classifies one token of a rule definition.
// Port of duckdb PEGTokenType (peg_parser.hpp).
type TokenType int

const (
	// Literal is a quoted literal token ('Keyword').
	Literal TokenType = iota
	// Reference is a reference to another rule (Rule).
	Reference
	// Operator is a grammar operator (/ ? * + ( ) !).
	Operator
	// FunctionCall is the start of a parameterized-rule call (List(...)).
	FunctionCall
	// Regex is a character-class token ([^'] or <[a-z_]i[a-z0-9_]i*>).
	Regex
)

// Token is one token of a rule definition.
type Token struct {
	Type TokenType
	Text string
}

// Rule is a parsed grammar rule: a name, optional parameters (for macros
// such as List(D)), and the flat token stream of its definition. Port of
// duckdb PEGRule.
type Rule struct {
	Name string
	// Parameters maps parameter name to position. Upstream supports at most
	// one parameter per rule; the matcher factory enforces that.
	Parameters map[string]int
	Tokens     []Token
}

// Grammar is an immutable set of parsed rules, keyed case-insensitively by
// rule name (upstream uses case_insensitive_map_t). Port of duckdb
// ParsedGrammar.
type Grammar struct {
	rules map[string]*Rule // key: lower-cased rule name
	names []string         // insertion order, original spelling
}

// Rule returns the rule with the given name (case-insensitive), or nil.
func (g *Grammar) Rule(name string) *Rule {
	return g.rules[strings.ToLower(name)]
}

// RuleNames returns all rule names in insertion order.
func (g *Grammar) RuleNames() []string {
	return append([]string(nil), g.names...)
}

func (g *Grammar) addRule(rule *Rule) error {
	key := strings.ToLower(rule.Name)
	if _, ok := g.rules[key]; ok {
		return fmt.Errorf("failed to parse grammar - duplicate rule name %s", rule.Name)
	}
	g.rules[key] = rule
	g.names = append(g.names, rule.Name)
	return nil
}

// Parse parses PEG grammar text into rules. It is a direct port of duckdb
// PEGParser::ParseRules (peg_parser.cpp): a small state machine over the
// characters of the grammar text that produces, per rule, a flat token
// stream. Structure (grouping, choice, repetition) is interpreted later by
// the matcher factory, exactly as upstream does.
func Parse(text string) (*Grammar, error) {
	p := &pegParser{grammar: &Grammar{rules: make(map[string]*Rule)}}
	if err := p.parseRules(text); err != nil {
		return nil, err
	}
	return p.grammar, nil
}

// ParseSingleRule parses text that must contain exactly one rule definition.
// Port of duckdb ParsedGrammar::ParseSingleRule.
func ParseSingleRule(text string) (*Rule, error) {
	g, err := Parse(text)
	if err != nil {
		return nil, err
	}
	if len(g.names) != 1 {
		return nil, fmt.Errorf("expected exactly one PEG rule definition")
	}
	return g.Rule(g.names[0]), nil
}

type parseState int

const (
	stateRuleName parseState = iota
	stateRuleSeparator
	stateRuleDefinition
)

type pegParser struct {
	grammar *Grammar
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func isNewline(c byte) bool {
	return c == '\n' || c == '\r'
}

func isAlphaNumeric(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func isPEGOperator(c byte) bool {
	switch c {
	case '/', '?', '(', ')', '*', '+', '!':
		return true
	default:
		return false
	}
}

func (p *pegParser) parseRules(grammar string) error {
	var ruleName string
	rule := &Rule{}
	state := stateRuleName
	bracketCount := 0
	inOrClause := false

	c := 0
	n := len(grammar)
	for c < n {
		if grammar[c] == '#' {
			// comment - ignore until EOL
			for c < n && !isNewline(grammar[c]) {
				c++
			}
			continue
		}
		if state == stateRuleDefinition && isNewline(grammar[c]) && bracketCount == 0 &&
			!inOrClause && len(rule.Tokens) > 0 {
			// a newline while parsing a rule definition completes the rule
			rule.Name = ruleName
			if err := p.grammar.addRule(rule); err != nil {
				return err
			}
			rule = &Rule{}
			ruleName = ""
			state = stateRuleName
			c++
			continue
		}
		if isSpace(grammar[c]) {
			c++
			continue
		}
		switch state {
		case stateRuleName:
			startPos := c
			if grammar[c] == '%' {
				// rules can start with % (%whitespace)
				c++
			}
			for c < n && isAlphaNumeric(grammar[c]) {
				c++
			}
			if c == startPos {
				return fmt.Errorf("failed to parse grammar - expected an alpha-numeric rule name (pos %d)", c)
			}
			ruleName = grammar[startPos:c]
			rule = &Rule{}
			state = stateRuleSeparator
		case stateRuleSeparator:
			if grammar[c] == '(' {
				if len(rule.Parameters) > 0 {
					return fmt.Errorf("failed to parse grammar - multiple parameters at position %d", c)
				}
				c++
				parameterStart := c
				for c < n && isAlphaNumeric(grammar[c]) {
					c++
				}
				if parameterStart == c {
					return fmt.Errorf("failed to parse grammar - expected a parameter at position %d", c)
				}
				if rule.Parameters == nil {
					rule.Parameters = make(map[string]int)
				}
				rule.Parameters[grammar[parameterStart:c]] = len(rule.Parameters)
				if c >= n || grammar[c] != ')' {
					return fmt.Errorf("failed to parse grammar - expected closing bracket at position %d", c)
				}
				c++
			} else {
				if c+1 >= n || grammar[c] != '<' || grammar[c+1] != '-' {
					return fmt.Errorf("failed to parse grammar - expected a rule definition (<-) (pos %d)", c)
				}
				c += 2
				state = stateRuleDefinition
			}
		case stateRuleDefinition:
			inOrClause = false
			switch {
			case grammar[c] == '\'':
				// parse literal
				c++
				literalStart := c
				for c < n && grammar[c] != '\'' {
					if grammar[c] == '\\' {
						// escape
						c++
					}
					c++
				}
				if c >= n {
					return fmt.Errorf("failed to parse grammar - did not find closing ' (pos %d)", c)
				}
				rule.Tokens = append(rule.Tokens, Token{Type: Literal, Text: grammar[literalStart:c]})
				c++
				if c < n && grammar[c] == 'i' {
					return fmt.Errorf("failed to parse grammar - unexpected \"i\" found in grammar near rule %s", ruleName)
				}
			case isAlphaNumeric(grammar[c]):
				// alphanumeric character - this is a rule reference
				ruleStart := c
				for c < n && isAlphaNumeric(grammar[c]) {
					c++
				}
				tok := Token{Text: grammar[ruleStart:c]}
				if c < n && grammar[c] == '(' {
					// this is a function call
					c++
					bracketCount++
					tok.Type = FunctionCall
				} else {
					tok.Type = Reference
				}
				rule.Tokens = append(rule.Tokens, tok)
			case grammar[c] == '[' || grammar[c] == '<':
				// character class - [^'] or <...>
				ruleStart := c
				finalChar := byte(']')
				if grammar[c] == '<' {
					finalChar = '>'
				}
				for c < n && grammar[c] != finalChar {
					if grammar[c] == '\\' {
						// handle escapes
						c++
					}
					if c < n {
						c++
					}
				}
				c++
				end := c
				if end > n {
					end = n
				}
				rule.Tokens = append(rule.Tokens, Token{Type: Regex, Text: grammar[ruleStart:end]})
			case isPEGOperator(grammar[c]):
				switch grammar[c] {
				case '(':
					bracketCount++
				case ')':
					if bracketCount == 0 {
						return fmt.Errorf("failed to parse grammar - unclosed bracket at position %d in rule %s", c, ruleName)
					}
					bracketCount--
				case '/':
					inOrClause = true
				}
				// operators are always length 1
				rule.Tokens = append(rule.Tokens, Token{Type: Operator, Text: grammar[c : c+1]})
				c++
			default:
				return fmt.Errorf("unrecognized rule contents in rule %s (character %s)", ruleName, grammar[c:c+1])
			}
		}
	}
	if state == stateRuleSeparator {
		return fmt.Errorf("failed to parse grammar - rule %s does not have a definition", ruleName)
	}
	if state == stateRuleDefinition {
		if len(rule.Tokens) == 0 {
			return fmt.Errorf("failed to parse grammar - rule %s is empty", ruleName)
		}
		rule.Name = ruleName
		if err := p.grammar.addRule(rule); err != nil {
			return err
		}
	}
	return nil
}
