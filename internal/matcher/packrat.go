package matcher

// packratCache memoizes (matcher, token position) match outcomes for the
// designated hot rules, killing exponential backtracking on pathological
// inputs. Port of ParserPackratCache (parser_packrat.cpp). One cache lives
// per top-level match call.
type packratCache struct {
	entries map[packratKey]packratEntry
}

type packratKey struct {
	matcherID  int
	tokenIndex int
}

type packratEntry struct {
	success      bool
	posAfter     int
	maxTokenSeen int
	result       ParseResult
}

func newPackratCache() *packratCache {
	return &packratCache{entries: make(map[packratKey]packratEntry)}
}

func (c *packratCache) match(m matcher, s *state) ParseResult {
	key := packratKey{matcherID: m.meta().id, tokenIndex: s.pos}
	if cached, ok := c.entries[key]; ok {
		s.pos = cached.posAfter
		if cached.maxTokenSeen > s.max {
			s.max = cached.maxTokenSeen
		}
		if cached.success {
			return cached.result
		}
		return nil
	}
	maxBefore := s.max
	r := m.matchInternal(s)
	entry := packratEntry{
		success:  r != nil,
		posAfter: s.pos,
		result:   r,
	}
	entry.maxTokenSeen = maxBefore
	if s.max > entry.maxTokenSeen {
		entry.maxTokenSeen = s.max
	}
	c.entries[key] = entry
	return r
}
