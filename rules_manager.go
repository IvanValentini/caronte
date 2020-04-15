package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/flier/gohs/hyperscan"
	"github.com/go-playground/validator/v10"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
)

const DirectionBoth = 0
const DirectionToServer = 1
const DirectionToClient = 2

type RegexFlags struct {
	Caseless        bool `json:"caseless" bson:"caseless,omitempty"`                 // Set case-insensitive matching.
	DotAll          bool `json:"dot_all" bson:"dot_all,omitempty"`                   // Matching a `.` will not exclude newlines.
	MultiLine       bool `json:"multi_line" bson:"multi_line,omitempty"`             // Set multi-line anchoring.
	SingleMatch     bool `json:"single_match" bson:"single_match,omitempty"`         // Set single-match only mode.
	Utf8Mode        bool `json:"utf_8_mode" bson:"utf_8_mode,omitempty"`             // Enable UTF-8 mode for this expression.
	UnicodeProperty bool `json:"unicode_property" bson:"unicode_property,omitempty"` // Enable Unicode property support for this expression
}

type Pattern struct {
	Regex           string     `json:"regex" binding:"min=1" bson:"regex"`
	Flags           RegexFlags `json:"flags" bson:"flags,omitempty"`
	MinOccurrences  uint       `json:"min_occurrences" bson:"min_occurrences,omitempty"`
	MaxOccurrences  uint       `json:"max_occurrences" binding:"omitempty,gtefield=MinOccurrences" bson:"max_occurrences,omitempty"`
	Direction       uint8      `json:"direction" binding:"omitempty,max=2" bson:"direction,omitempty"`
	internalID      int
}

type Filter struct {
	ServicePort   uint16 `json:"service_port" bson:"service_port,omitempty"`
	ClientAddress string `json:"client_address" binding:"omitempty,ip_addr" bson:"client_address,omitempty"`
	ClientPort    uint16 `json:"client_port" bson:"client_port,omitempty"`
	MinDuration   uint   `json:"min_duration" bson:"min_duration,omitempty"`
	MaxDuration   uint   `json:"max_duration" binding:"omitempty,gtefield=MinDuration" bson:"max_duration,omitempty"`
	MinBytes      uint   `json:"min_bytes" bson:"min_bytes,omitempty"`
	MaxBytes      uint   `json:"max_bytes" binding:"omitempty,gtefield=MinBytes" bson:"max_bytes,omitempty"`
}

type Rule struct {
	ID       RowID     `json:"id" bson:"_id,omitempty"`
	Name     string    `json:"name" binding:"min=3" bson:"name"`
	Color    string    `json:"color" binding:"hexcolor" bson:"color"`
	Notes    string    `json:"notes" bson:"notes,omitempty"`
	Enabled  bool      `json:"enabled" bson:"enabled"`
	Patterns []Pattern `json:"patterns" bson:"patterns"`
	Filter   Filter    `json:"filter" bson:"filter,omitempty"`
	Version  int64     `json:"version" bson:"version"`
}

type RulesDatabase struct {
	database     hyperscan.StreamDatabase
	databaseSize int
	version      RowID
}

type RulesManager interface {
	LoadRules() error
	AddRule(context context.Context, rule Rule) (RowID, error)
	GetRule(id RowID) (Rule, bool)
	UpdateRule(context context.Context, id RowID, rule Rule) (bool, error)
	GetRules() []Rule
	FillWithMatchedRules(connection *Connection, clientMatches map[uint][]PatternSlice, serverMatches map[uint][]PatternSlice)
	DatabaseUpdateChannel() chan RulesDatabase
}

type rulesManagerImpl struct {
	storage         Storage
	rules           map[RowID]Rule
	rulesByName     map[string]Rule
	patterns        []*hyperscan.Pattern
	patternsIds     map[string]int
	mutex           sync.Mutex
	databaseUpdated chan RulesDatabase
	validate        *validator.Validate
}

func NewRulesManager(storage Storage) RulesManager {
	return &rulesManagerImpl{
		storage:         storage,
		rules:           make(map[RowID]Rule),
		rulesByName:     make(map[string]Rule),
		patterns:        make([]*hyperscan.Pattern, 0),
		patternsIds:     make(map[string]int),
		mutex:           sync.Mutex{},
		databaseUpdated: make(chan RulesDatabase, 1),
		validate:        validator.New(),
	}
}

func (rm *rulesManagerImpl) LoadRules() error {
	var rules []Rule
	if err := rm.storage.Find(Rules).Sort("_id", true).All(&rules); err != nil {
		return err
	}

	for _, rule := range rules {
		if err := rm.validateAndAddRuleLocal(&rule); err != nil {
			log.WithError(err).WithField("rule", rule).Warn("failed to import rule")
		}
	}

	return rm.generateDatabase(rules[len(rules)-1].ID)
}

func (rm *rulesManagerImpl) AddRule(context context.Context, rule Rule) (RowID, error) {
	rm.mutex.Lock()

	rule.ID = CustomRowID(uint64(len(rm.rules)), time.Now())
	rule.Enabled = true

	if err := rm.validateAndAddRuleLocal(&rule); err != nil {
		rm.mutex.Unlock()
		return EmptyRowID(), err
	}

	if err := rm.generateDatabase(rule.ID); err != nil {
		rm.mutex.Unlock()
		log.WithError(err).WithField("rule", rule).Panic("failed to generate database")
	}
	rm.mutex.Unlock()

	if _, err := rm.storage.Insert(Rules).Context(context).One(rule); err != nil {
		log.WithError(err).WithField("rule", rule).Panic("failed to insert rule on database")
	}

	return rule.ID, nil
}

func (rm *rulesManagerImpl) GetRule(id RowID) (Rule, bool) {
	rule, isPresent := rm.rules[id]
	return rule, isPresent
}

func (rm *rulesManagerImpl) UpdateRule(context context.Context, id RowID, rule Rule) (bool, error) {
	newRule, isPresent := rm.rules[id]
	if !isPresent {
		return false, nil
	}

	sameName, isPresent := rm.rulesByName[rule.Name]
	if isPresent && sameName.ID != id {
		return false, errors.New("already exists another rule with the same name")
	}

	updated, err := rm.storage.Update(Rules).Context(context).Filter(OrderedDocument{{"_id", id}}).
		One(UnorderedDocument{"name": rule.Name, "color": rule.Color})
	if err != nil {
		log.WithError(err).WithField("rule", rule).Panic("failed to update rule on database")
	}

	if updated {
		rm.mutex.Lock()
		newRule.Name = rule.Name
		newRule.Color = rule.Color

		delete(rm.rulesByName, newRule.Name)
		rm.rulesByName[rule.Name] = newRule
		rm.rules[rule.ID] = newRule
		rm.mutex.Unlock()
	}

	return updated, nil
}

func (rm *rulesManagerImpl) GetRules() []Rule {
	rules := make([]Rule, 0, len(rm.rules))

	for _, rule := range rm.rules {
		rules = append(rules, rule)
	}

	return rules
}

func (rm *rulesManagerImpl) SetFlag(context context.Context, flagRegex string) error {
	_, err := rm.AddRule(context, Rule{
		Name:  "flag",
		Color: "#ff0000",
		Notes: "Mark connections where the flag is stolen",
		Patterns: []Pattern{
			{Regex: flagRegex, Direction: DirectionToClient},
		},
	})

	return err
}

func (rm *rulesManagerImpl) FillWithMatchedRules(connection *Connection, clientMatches map[uint][]PatternSlice,
	serverMatches map[uint][]PatternSlice) {
}

func (rm *rulesManagerImpl) DatabaseUpdateChannel() chan RulesDatabase {
	return rm.databaseUpdated
}

func (rm *rulesManagerImpl) validateAndAddRuleLocal(rule *Rule) error {
	if _, alreadyPresent := rm.rulesByName[rule.Name]; alreadyPresent {
		return errors.New("rule name must be unique")
	}

	newPatterns := make([]*hyperscan.Pattern, 0, len(rule.Patterns))
	duplicatePatterns := make(map[string]bool)
	for i, pattern := range rule.Patterns {
		if err := rm.validate.Struct(pattern); err != nil {
			return err
		}

		compiledPattern, err := pattern.BuildPattern()
		if err != nil {
			return err
		}
		regex := compiledPattern.String()
		if _, isPresent := duplicatePatterns[regex]; isPresent {
			return errors.New("duplicate pattern")
		}
		if existingPattern, isPresent := rm.patternsIds[regex]; isPresent {
			rule.Patterns[i].internalID = existingPattern
			continue
		}

		id := len(rm.patternsIds) + len(newPatterns)
		rule.Patterns[i].internalID = id
		compiledPattern.Id = id
		newPatterns = append(newPatterns, compiledPattern)
		duplicatePatterns[regex] = true
	}

	startId := len(rm.patterns)
	for id, pattern := range newPatterns {
		rm.patterns = append(rm.patterns, pattern)
		rm.patternsIds[pattern.String()] = startId + id
	}

	rm.rules[rule.ID] = *rule
	rm.rulesByName[rule.Name] = *rule

	return nil
}

func (rm *rulesManagerImpl) generateDatabase(version RowID) error {
	database, err := hyperscan.NewStreamDatabase(rm.patterns...)
	if err != nil {
		return err
	}

	rm.databaseUpdated <- RulesDatabase{
		database:     database,
		databaseSize: len(rm.patterns),
		version:      version,
	}

	return nil
}

func (p *Pattern) BuildPattern() (*hyperscan.Pattern, error) {
	hp, err := hyperscan.ParsePattern(fmt.Sprintf("/%s/", p.Regex))
	if err != nil {
		return nil, err
	}

	if p.Flags.Caseless {
		hp.Flags |= hyperscan.Caseless
	}
	if p.Flags.DotAll {
		hp.Flags |= hyperscan.DotAll
	}
	if p.Flags.MultiLine {
		hp.Flags |= hyperscan.MultiLine
	}
	if p.Flags.SingleMatch {
		hp.Flags |= hyperscan.SingleMatch
	}
	if p.Flags.Utf8Mode {
		hp.Flags |= hyperscan.Utf8Mode
	}
	if p.Flags.UnicodeProperty {
		hp.Flags |= hyperscan.UnicodeProperty
	}

	if !hp.IsValid() {
		return nil, errors.New("can't validate the pattern")
	}

	return hp, nil
}
