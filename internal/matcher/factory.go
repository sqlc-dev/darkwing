package matcher

import (
	"fmt"
	"strings"

	"github.com/sqlc-dev/darkwing/internal/grammar"
)

// factory compiles grammar rules into a matcher tree. Port of
// MatcherFactory (matcher_factory.cpp): the flat token stream of each rule
// is interpreted with a stack of open lists; `/` `?` `*` `+` rewrite the
// most recent element, replicating upstream's (deliberately ported)
// binding quirks — `/` binds only the immediately preceding element, which
// is why upstream grammars parenthesize multi-token alternatives.
type factory struct {
	grammar *grammar.Grammar
	// matchers caches rule matchers by rule name and holds the rule
	// overrides (upstream: string_map_t<reference<Matcher>>, case
	// sensitive).
	matchers map[string]matcher
	// keywords caches keyword matchers case-insensitively.
	keywords map[string]*keywordMatcher
	// memoized is the set of packrat-memoized rule names.
	memoized map[string]bool
	nextID   int
}

func newFactory(g *grammar.Grammar) *factory {
	return &factory{
		grammar:  g,
		matchers: make(map[string]matcher),
		keywords: make(map[string]*keywordMatcher),
		memoized: make(map[string]bool),
	}
}

// alloc assigns the packrat identity (upstream: MatcherAllocator).
func (f *factory) alloc(m matcher) matcher {
	m.meta().id = f.nextID
	f.nextID++
	return m
}

func (f *factory) newList() *listMatcher {
	m := &listMatcher{}
	f.alloc(m)
	return m
}

func (f *factory) newListWith(children []matcher) *listMatcher {
	m := &listMatcher{children: children}
	f.alloc(m)
	return m
}

func (f *factory) newChoice(children []matcher) *choiceMatcher {
	m := &choiceMatcher{children: children}
	f.alloc(m)
	return m
}

func (f *factory) newOptional(child matcher) *optionalMatcher {
	m := &optionalMatcher{child: child}
	f.alloc(m)
	return m
}

func (f *factory) newRepeat(element matcher) *repeatMatcher {
	m := &repeatMatcher{element: element}
	f.alloc(m)
	return m
}

func (f *factory) keyword(text string) *keywordMatcher {
	key := strings.ToLower(text)
	if m, ok := f.keywords[key]; ok {
		return m
	}
	m := &keywordMatcher{keyword: text}
	f.alloc(m)
	f.keywords[key] = m
	return m
}

// addRuleOverride installs a special-token matcher for a rule, replacing
// its grammar definition. Port of MatcherFactory::AddRuleOverride.
func (f *factory) addRuleOverride(name string, m matcher) {
	f.alloc(m)
	if f.memoized[name] {
		m.meta().memoized = true
	}
	if f.grammar.Rule(name) != nil {
		m.meta().setRule(name)
	} else if m.meta().name == "" {
		m.meta().name = name
	}
	f.matchers[name] = m
}

// stack entry for interpreting one rule's token stream. Port of
// MatcherFactory::MatcherList.
type stackEntry struct {
	m            matcher
	functionName string
	isFunction   bool
}

type matcherStack struct {
	f       *factory
	entries []stackEntry
}

func (ms *matcherStack) addRoot(m matcher) {
	ms.entries = append(ms.entries, stackEntry{m: m})
}

func (ms *matcherStack) beginFunction(name string) {
	ms.entries = append(ms.entries, stackEntry{m: ms.f.newList(), functionName: name, isFunction: true})
}

func (ms *matcherStack) addMatcher(m matcher) error {
	root := ms.entries[len(ms.entries)-1].m
	switch root := root.(type) {
	case *listMatcher:
		root.children = append(root.children, m)
	case *choiceMatcher:
		// for a choice matcher we pop the choice from the stack after
		// adding the alternative
		if len(ms.entries) <= 1 {
			return fmt.Errorf("choice matcher should never be the root in the matcher stack")
		}
		root.children = append(root.children, m)
		ms.entries = ms.entries[:len(ms.entries)-1]
	default:
		return fmt.Errorf("cannot add matcher to root matcher of this type")
	}
	return nil
}

func (ms *matcherStack) closeBracket() error {
	if len(ms.entries) <= 1 {
		return fmt.Errorf("PEG matcher create error - found too many close brackets")
	}
	top := ms.entries[len(ms.entries)-1]
	if !top.isFunction {
		ms.entries = ms.entries[:len(ms.entries)-1]
		return ms.addMatcher(top.m)
	}
	// function matcher: substitute the parameterized rule
	params := top.m.(*listMatcher)
	var param matcher
	if len(params.children) == 1 {
		param = params.children[0]
	} else {
		param = ms.f.newListWith(params.children)
	}
	call, err := ms.f.createMatcher(top.functionName, []matcher{param})
	if err != nil {
		return err
	}
	ms.entries = ms.entries[:len(ms.entries)-1]
	return ms.addMatcher(call)
}

func (ms *matcherStack) lastRootList() (*listMatcher, error) {
	root, ok := ms.entries[len(ms.entries)-1].m.(*listMatcher)
	if !ok {
		return nil, fmt.Errorf("expected a list matcher")
	}
	return root, nil
}

// createMatcher builds (and caches) the matcher for a rule, recursively
// building referenced rules. params carries macro arguments for
// parameterized rules (List(D), Parens(D)). Port of
// MatcherFactory::CreateMatcher.
func (f *factory) createMatcher(ruleName string, params []matcher) (matcher, error) {
	isFunctionCall := len(params) > 0
	if !isFunctionCall {
		if m, ok := f.matchers[ruleName]; ok {
			return m, nil
		}
	}
	rule := f.grammar.Rule(ruleName)
	if rule == nil {
		return nil, fmt.Errorf("failed to create matcher for rule %s - rule is missing", ruleName)
	}
	// matchers can be recursive: cache before recursively constructing
	root := f.newList()
	if !isFunctionCall {
		f.matchers[rule.Name] = root
	}

	stack := &matcherStack{f: f}
	stack.addRoot(root)
	if len(rule.Parameters) > 1 {
		return nil, fmt.Errorf("only functions with a single parameter are supported")
	}
	if len(params) != len(rule.Parameters) {
		return nil, fmt.Errorf("parameter count mismatch (rule %s expected %d parameters but got %d)",
			ruleName, len(rule.Parameters), len(params))
	}
	for _, tok := range rule.Tokens {
		var err error
		switch tok.Type {
		case grammar.Literal:
			err = stack.addMatcher(f.keyword(tok.Text))
		case grammar.Reference:
			if idx, ok := rule.Parameters[tok.Text]; ok {
				// refers to a macro parameter - use it directly
				err = stack.addMatcher(params[idx])
			} else {
				var child matcher
				child, err = f.createMatcher(tok.Text, nil)
				if err == nil {
					err = stack.addMatcher(child)
				}
			}
		case grammar.FunctionCall:
			stack.beginFunction(tok.Text)
		case grammar.Operator:
			err = f.applyOperator(stack, ruleName, tok.Text[0])
		case grammar.Regex:
			err = fmt.Errorf("REGEX operator not supported in PEG grammar (found in rule %s)", ruleName)
		default:
			err = fmt.Errorf("unrecognized peg token type")
		}
		if err != nil {
			return nil, err
		}
	}
	if len(stack.entries) != 1 {
		return nil, fmt.Errorf("PEG matcher create error - unclosed bracket found in rule %s", ruleName)
	}

	root.setRule(rule.Name)
	if f.memoized[ruleName] {
		root.memoized = true
	}
	return root, nil
}

func (f *factory) applyOperator(stack *matcherStack, ruleName string, op byte) error {
	switch op {
	case '?', '*':
		// optional/repeat rewrites the most recent element of the current
		// list; '*' is Optional(Repeat(child))
		root, err := stack.lastRootList()
		if err != nil {
			return fmt.Errorf("optional/repeat in rule %s: %w", ruleName, err)
		}
		if len(root.children) == 0 {
			return fmt.Errorf("optional/repeat rule found as first token in rule %s", ruleName)
		}
		final := root.children[len(root.children)-1]
		if op == '*' {
			final = f.newRepeat(final)
		}
		root.children[len(root.children)-1] = f.newOptional(final)
	case '+':
		root, err := stack.lastRootList()
		if err != nil {
			return fmt.Errorf("repeat in rule %s: %w", ruleName, err)
		}
		if len(root.children) == 0 {
			return fmt.Errorf("repeat rule found as first token in rule %s", ruleName)
		}
		final := root.children[len(root.children)-1]
		root.children[len(root.children)-1] = f.newRepeat(final)
	case '/':
		// ordered choice between the most recent element and what follows
		root, err := stack.lastRootList()
		if err != nil {
			return fmt.Errorf("OR in rule %s: %w", ruleName, err)
		}
		if len(root.children) == 0 {
			return fmt.Errorf("OR rule found as first token in rule %s", ruleName)
		}
		previous := root.children[len(root.children)-1]
		if choice, ok := previous.(*choiceMatcher); ok {
			stack.addRoot(choice)
		} else {
			choice := f.newChoice([]matcher{previous})
			root.children[len(root.children)-1] = choice
			stack.addRoot(choice)
		}
	case '(':
		stack.addRoot(f.newList())
	case ')':
		return stack.closeBracket()
	case '!':
		// negative lookahead is parsed but ignored, replicating the pinned
		// DuckDB build (an acknowledged upstream TODO). Flip this in
		// lockstep with upstream, never independently.
	default:
		return fmt.Errorf("unrecognized peg operator type %q", string(op))
	}
	return nil
}

//===----------------------------------------------------------------------===//
// Generated tables (transcribed from matcher_factory.cpp)
//===----------------------------------------------------------------------===//

// packratMemoizedRules is upstream's designated hot-rule list
// (START/END GENERATED PACKRAT MEMOIZED RULES).
var packratMemoizedRules = []string{
	"Expression",
	"LambdaArrowExpression",
	"LogicalOrExpression",
	"LogicalAndExpression",
	"LogicalNotExpression",
	"IsExpression",
	"ComparisonExpression",
	"BitwiseExpression",
	"AdditiveExpression",
	"MultiplicativeExpression",
	"ExponentiationExpression",
	"PrefixExpression",
	"CollateExpression",
	"AtTimeZoneExpression",
	"SingleExpression",
	"BaseExpression",
	"ParensExpression",
	"ParenthesisExpression",
	"Identifier",
	"ColId",
	"ColumnReference",
	"FunctionExpression",
}

// installOverrides transcribes upstream's rule override table
// (START/END GENERATED RULE OVERRIDES + the EndOfInput override). Special
// tokens are handled natively rather than by grammar expansion.
func (f *factory) installOverrides() {
	ident := func(class identifierClass, supportsString bool, display string) matcher {
		return &identifierMatcher{class: class, supportsStringLiteral: supportsString, display: display}
	}
	reserved := func(supportsString bool, display string) matcher {
		return &identifierMatcher{reserved: true, supportsStringLiteral: supportsString, display: display}
	}

	f.addRuleOverride("Identifier", ident(classColumnName, false, "VARIABLE"))
	f.addRuleOverride("ReservedIdentifier", reserved(false, "VARIABLE"))
	f.addRuleOverride("CatalogName", ident(classColumnName, false, "CATALOG_NAME"))
	f.addRuleOverride("SchemaName", ident(classColumnName, false, "SCHEMA_NAME"))
	f.addRuleOverride("ReservedSchemaName", reserved(false, "SCHEMA_NAME"))
	f.addRuleOverride("TableName", ident(classColumnName, true, "TABLE_NAME"))
	f.addRuleOverride("ReservedTableName", reserved(true, "TABLE_NAME"))
	f.addRuleOverride("ColumnName", ident(classColumnName, false, "COLUMN_NAME"))
	f.addRuleOverride("ReservedColumnName", reserved(false, "COLUMN_NAME"))
	f.addRuleOverride("IndexName", ident(classColumnName, false, "VARIABLE"))
	f.addRuleOverride("ReservedIndexName", reserved(false, "VARIABLE"))
	f.addRuleOverride("SequenceName", ident(classColumnName, false, "VARIABLE"))
	f.addRuleOverride("FunctionName", ident(classTypeFunc, false, "SCALAR_FUNCTION_NAME"))
	f.addRuleOverride("ReservedFunctionName", reserved(false, "SCALAR_FUNCTION_NAME"))
	f.addRuleOverride("ReservedKeyword", reserved(false, "VARIABLE"))
	f.addRuleOverride("TableFunctionName", ident(classTypeFunc, false, "TABLE_FUNCTION_NAME"))
	f.addRuleOverride("TypeName", ident(classTypeName, false, "TYPE_NAME"))
	f.addRuleOverride("ReservedTypeName", reserved(false, "TYPE_NAME"))
	f.addRuleOverride("PragmaName", ident(classColumnName, false, "PRAGMA_NAME"))
	f.addRuleOverride("SettingName", ident(classColumnName, false, "SETTING_NAME"))
	f.addRuleOverride("CopyOptionName", reserved(false, "VARIABLE"))
	f.addRuleOverride("NumberLiteral", &numberLiteralMatcher{matcherMeta: matcherMeta{name: "NumberLiteral"}})
	f.addRuleOverride("StringLiteral", &stringLiteralMatcher{matcherMeta: matcherMeta{name: "StringLiteral"}})
	f.addRuleOverride("OperatorLiteral", &operatorMatcher{})

	// EndOfInput has no grammar body; satisfied here, as upstream does
	// outside the regenerated block
	f.addRuleOverride("EndOfInput", &endOfInputMatcher{})
}

// OverrideRuleNames returns the names of all rule overrides, for tests that
// cross-check the override table against the vendored grammar.
func OverrideRuleNames() []string {
	return []string{
		"Identifier", "ReservedIdentifier", "CatalogName", "SchemaName",
		"ReservedSchemaName", "TableName", "ReservedTableName", "ColumnName",
		"ReservedColumnName", "IndexName", "ReservedIndexName", "SequenceName",
		"FunctionName", "ReservedFunctionName", "ReservedKeyword",
		"TableFunctionName", "TypeName", "ReservedTypeName", "PragmaName",
		"SettingName", "CopyOptionName", "NumberLiteral", "StringLiteral",
		"OperatorLiteral", "EndOfInput",
	}
}
