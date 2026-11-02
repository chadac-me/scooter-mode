package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request kinds Scooter's contribs attach to an interrupt's metadata.
const (
	kindQuestions = "ask-user.questions"
	kindAWS       = "aws.permissions"
)

// question is one of Claude's AskUserQuestion questions.
type question struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
		Preview     string `json:"preview"`
	} `json:"options"`
}

// questionsOf returns the request's answerable questions, or nil.
func questionsOf(data json.RawMessage) []question {
	var d struct {
		Questions []question `json:"questions"`
	}
	if json.Unmarshal(data, &d) != nil {
		return nil
	}
	var qs []question
	for _, q := range d.Questions {
		if q.Question != "" && len(q.Options) > 0 {
			qs = append(qs, q)
		}
	}
	return qs
}

func fieldKey(i int) string { return fmt.Sprintf("question_%d", i) }

// questionSchema asks the questions as an ACP form, one select each.
func questionSchema(qs []question) map[string]any {
	props := orderedProps{}
	var required []string
	for i, q := range qs {
		var opts []any
		for _, o := range q.Options {
			opt := map[string]any{"const": o.Label, "title": o.Label}
			if o.Description != "" {
				opt["description"] = o.Description
			}
			if o.Preview != "" {
				// agent-shell reads previews only under Claude's key.
				opt["_meta"] = map[string]any{"_claude/askUserQuestionOption": map[string]any{"preview": o.Preview}}
			}
			opts = append(opts, opt)
		}
		field := map[string]any{"type": "string", "title": q.Question, "oneOf": opts}
		if q.MultiSelect {
			field = map[string]any{"type": "array", "title": q.Question, "items": map[string]any{"anyOf": opts}}
		}
		// The header is the heading, as the web UI's chip.
		if q.Header != "" {
			field["title"], field["description"] = q.Header, q.Question
		}
		key := fieldKey(i)
		props = append(props, prop{key, field})
		// The marker folds this into the select as an "Other" choice.
		props = append(props, prop{key + "_custom", map[string]any{
			"type":  "string",
			"title": "Other",
			"_meta": map[string]any{"_askUserQuestionCustomAnswer": map[string]any{"questionId": key}},
		}})
		required = append(required, key)
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// formAnswers maps a form's content to AskUserQuestion's answers, by question.
func formAnswers(qs []question, content map[string]any) (map[string]string, bool) {
	answers := map[string]string{}
	for i, q := range qs {
		key := fieldKey(i)
		var a string
		if s, ok := content[key+"_custom"].(string); ok && strings.TrimSpace(s) != "" {
			a = strings.TrimSpace(s)
		} else {
			switch v := content[key].(type) {
			case string:
				a = v
			case []any:
				var picks []string
				for _, p := range v {
					if s, ok := p.(string); ok {
						picks = append(picks, s)
					}
				}
				a = strings.Join(picks, ", ")
			}
		}
		if a == "" {
			return nil, false
		}
		answers[q.Question] = a
	}
	return answers, true
}

// questionsText is the questions as markdown, for clients without forms.
func questionsText(qs []question) string {
	var b strings.Builder
	for _, q := range qs {
		fmt.Fprintf(&b, "\n\n**%s**", q.Question)
		for _, o := range q.Options {
			fmt.Fprintf(&b, "\n- %s", o.Label)
			if o.Description != "" {
				fmt.Fprintf(&b, ": %s", o.Description)
			}
		}
	}
	return b.String()
}

// grant is the AWS broker's approval_data.
type grant struct {
	Account           string          `json:"account"`
	Risk              string          `json:"risk"`
	Justification     string          `json:"justification"`
	Summary           string          `json:"summary"`
	PolicyDocument    json.RawMessage `json:"policy_document"`
	ManagedPolicyArns []string        `json:"managed_policy_arns"`
	WriteActions      []string        `json:"write_actions"`
	ParentRequestID   string          `json:"parent_request_id"`
	Obligations       []struct {
		Rules      []string `json:"rules"`
		Parts      []string `json:"parts"`
		ApprovedBy string   `json:"approved_by"`
	} `json:"obligations"`
}

// statement is one IAM policy statement, its string-or-list fields as lists.
type statement struct {
	Effect    string
	Actions   []string
	Resources []string
	Condition json.RawMessage
}

// stringList reads IAM's string-or-list fields.
func stringList(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

func statementsOf(policy json.RawMessage) []statement {
	var doc struct {
		Statement json.RawMessage
	}
	if json.Unmarshal(policy, &doc) != nil || len(doc.Statement) == 0 {
		return nil
	}
	var raws []json.RawMessage
	// IAM allows one statement object in place of the list.
	if json.Unmarshal(doc.Statement, &raws) != nil {
		raws = []json.RawMessage{doc.Statement}
	}
	var out []statement
	for _, r := range raws {
		var s struct {
			Effect    string
			Action    json.RawMessage
			Resource  json.RawMessage
			Condition json.RawMessage
		}
		if json.Unmarshal(r, &s) != nil {
			continue
		}
		if s.Effect == "" {
			s.Effect = "Allow"
		}
		out = append(out, statement{s.Effect, stringList(s.Action), stringList(s.Resource), s.Condition})
	}
	return out
}

// awsText renders a grant as markdown, as the web UI's detail card does.
func awsText(data json.RawMessage) (title, body string) {
	var g grant
	if json.Unmarshal(data, &g) != nil || g.Account == "" || g.Risk == "" {
		return "", ""
	}
	title = fmt.Sprintf("AWS access to %s (%s risk)", g.Account, g.Risk)
	if g.ParentRequestID != "" {
		title += ", extending " + g.ParentRequestID
	}
	var b strings.Builder
	if g.Summary != "" {
		fmt.Fprintf(&b, "\n\n%s", g.Summary)
	}
	if g.Justification != "" {
		fmt.Fprintf(&b, "\n\nReason: %s", g.Justification)
	}
	writes := map[string]bool{}
	for _, a := range g.WriteActions {
		writes[a] = true
	}
	if len(g.WriteActions) > 0 {
		fmt.Fprintf(&b, "\n\n**Can change things:** `%s`", strings.Join(g.WriteActions, "`, `"))
	}
	if stmts := statementsOf(g.PolicyDocument); len(stmts) > 0 {
		b.WriteString("\n\n**Policy**")
		for _, s := range stmts {
			fmt.Fprintf(&b, "\n\n%s on `%s`", s.Effect, strings.Join(s.Resources, "`, `"))
			for _, a := range s.Actions {
				fmt.Fprintf(&b, "\n- `%s`", a)
				if writes[a] {
					b.WriteString(" (write)")
				}
			}
			if len(s.Condition) > 0 && string(s.Condition) != "null" {
				fmt.Fprintf(&b, "\n- when `%s`", s.Condition)
			}
		}
	}
	if len(g.ManagedPolicyArns) > 0 {
		b.WriteString("\n\n**Managed policies**")
		for _, arn := range g.ManagedPolicyArns {
			fmt.Fprintf(&b, "\n- `%s`", arn)
		}
	}
	if len(g.Obligations) > 0 {
		b.WriteString("\n\n**Approvals needed**")
		for _, o := range g.Obligations {
			status := "waiting"
			if o.ApprovedBy != "" {
				status = "approved by " + o.ApprovedBy
			}
			fmt.Fprintf(&b, "\n- %s: %s", strings.Join(o.Rules, ", "), status)
		}
	}
	return title, strings.TrimSpace(b.String())
}

// describe returns a permission dialog's title and markdown body.
func describe(in interrupt) (title, body string) {
	if in.Metadata.Kind == kindAWS {
		if title, body := awsText(in.Metadata.Data); title != "" {
			return title, body
		}
	}
	return in.Message, ""
}

// prop is one schema property; orderedProps keeps the form's field order.
type prop struct {
	key   string
	value any
}

type orderedProps []prop

func (p orderedProps) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, kv := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.key)
		v, err := json.Marshal(kv.value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}
