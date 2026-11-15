package services

import (
	"db-proxy/internal/core/domain"
	"encoding/json"
	"math/rand"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ChaosService manages and evaluates active fault injection/chaos rules.
type ChaosService struct {
	filePath string
	mu       sync.RWMutex
	rules    []domain.ChaosRule
	regexes  map[string]*regexp.Regexp
	rng      *rand.Rand
}

// NewChaosService instantiates a new ChaosService and loads existing rules from filePath.
func NewChaosService(filePath string) *ChaosService {
	cs := &ChaosService{
		filePath: filePath,
		regexes:  make(map[string]*regexp.Regexp),
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	_ = cs.loadRules()
	return cs
}

// loadRules reads rules from disk and builds the regex compilation cache.
func (cs *ChaosService) loadRules() error {
	data, err := os.ReadFile(cs.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// If file does not exist, initialize with empty rules list
			cs.rules = []domain.ChaosRule{}
			return nil
		}
		return err
	}

	var rules []domain.ChaosRule
	if err := json.Unmarshal(data, &rules); err != nil {
		return err
	}

	cs.rules = rules
	cs.rebuildRegexCache()
	return nil
}

// rebuildRegexCache compiles regexes for regex-type rules.
func (cs *ChaosService) rebuildRegexCache() {
	cs.regexes = make(map[string]*regexp.Regexp)
	for _, rule := range cs.rules {
		if rule.MatchType == "regex" && rule.Enabled {
			re, err := regexp.Compile(rule.Pattern)
			if err == nil {
				cs.regexes[rule.ID] = re
			}
		}
	}
}

// GetRules returns a copy of current rules.
func (cs *ChaosService) GetRules() []domain.ChaosRule {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	// Deep copy to prevent race conditions
	copied := make([]domain.ChaosRule, len(cs.rules))
	copy(copied, cs.rules)
	return copied
}

// UpdateRules overwrites current rules, saves them to disk, and rebuilds regex cache.
func (cs *ChaosService) UpdateRules(newRules []domain.ChaosRule) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Standardize IDs if empty
	for i, r := range newRules {
		if r.ID == "" {
			newRules[i].ID = time.Now().Format("20060102150405.000000")
		}
	}

	data, err := json.MarshalIndent(newRules, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(cs.filePath, data, 0644); err != nil {
		return err
	}

	cs.rules = newRules
	cs.rebuildRegexCache()
	return nil
}

// Evaluate checks if the incoming query/command matches any active rules.
// If a match is found, it returns the matched ChaosRule and a true boolean.
func (cs *ChaosService) Evaluate(dbType string, query string) (rule domain.ChaosRule, matched bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()

	trimmedQuery := strings.TrimSpace(query)
	lowerQuery := strings.ToLower(trimmedQuery)

	for _, r := range cs.rules {
		if !r.Enabled {
			continue
		}

		// Match service type (e.g. postgres, mariadb, redis). Empty means matches all services.
		if r.Service != "" && strings.ToLower(r.Service) != strings.ToLower(dbType) {
			continue
		}

		// Perform pattern matching
		match := false
		lowerPattern := strings.ToLower(r.Pattern)

		switch r.MatchType {
		case "exact":
			match = lowerQuery == lowerPattern

		case "substring":
			match = strings.Contains(lowerQuery, lowerPattern)

		case "regex":
			re, ok := cs.regexes[r.ID]
			if ok {
				match = re.MatchString(trimmedQuery)
			} else {
				// Fallback to substring in case regex compile failed
				match = strings.Contains(lowerQuery, lowerPattern)
			}
		}

		if match {
			// Evaluate probability
			if cs.rng.Float64() < r.Probability {
				return r, true // Trigger first matching rule
			}
		}
	}

	return domain.ChaosRule{}, false
}
