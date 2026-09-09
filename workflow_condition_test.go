package main

import (
	"fmt"
	"strings"
)

type workflowConditionContext struct {
	EventName      string
	ReleaseTagName string
	Cancelled      bool
	Needs          map[string]string
}

func evaluateWorkflowCondition(expression string, context workflowConditionContext) (bool, error) {
	expression = strings.TrimSpace(expression)
	if strings.HasPrefix(expression, "${{") && strings.HasSuffix(expression, "}}") {
		expression = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(expression, "${{"), "}}"))
	}
	return evaluateWorkflowBoolean(expression, context)
}

func evaluateWorkflowBoolean(expression string, context workflowConditionContext) (bool, error) {
	expression, err := trimWorkflowParentheses(strings.TrimSpace(expression))
	if err != nil {
		return false, err
	}
	if expression == "" {
		return false, fmt.Errorf("empty workflow condition")
	}

	for _, operator := range []string{"||", "&&"} {
		parts, found, err := splitWorkflowCondition(expression, operator)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		result := operator == "&&"
		for _, part := range parts {
			value, err := evaluateWorkflowBoolean(part, context)
			if err != nil {
				return false, err
			}
			if operator == "||" {
				result = result || value
			} else {
				result = result && value
			}
		}
		return result, nil
	}

	if strings.HasPrefix(expression, "!") {
		value, err := evaluateWorkflowBoolean(strings.TrimSpace(strings.TrimPrefix(expression, "!")), context)
		return !value, err
	}
	if expression == "cancelled()" {
		return context.Cancelled, nil
	}

	for _, operator := range []string{"==", "!="} {
		parts, found, err := splitWorkflowCondition(expression, operator)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		if len(parts) != 2 {
			return false, fmt.Errorf("workflow comparison %q has %d operands", expression, len(parts))
		}
		left, err := workflowConditionValue(parts[0], context)
		if err != nil {
			return false, err
		}
		right, err := workflowConditionValue(parts[1], context)
		if err != nil {
			return false, err
		}
		if operator == "==" {
			return left == right, nil
		}
		return left != right, nil
	}

	return false, fmt.Errorf("unsupported workflow condition %q", expression)
}

func workflowConditionValue(expression string, context workflowConditionContext) (string, error) {
	expression = strings.TrimSpace(expression)
	if len(expression) >= 2 && expression[0] == '\'' && expression[len(expression)-1] == '\'' {
		return strings.ReplaceAll(expression[1:len(expression)-1], "''", "'"), nil
	}
	switch expression {
	case "github.event_name":
		return context.EventName, nil
	case "github.event.release.tag_name":
		return context.ReleaseTagName, nil
	}
	if strings.HasPrefix(expression, "needs.") && strings.HasSuffix(expression, ".result") {
		name := strings.TrimSuffix(strings.TrimPrefix(expression, "needs."), ".result")
		if name != "" && !strings.Contains(name, ".") {
			return context.Needs[name], nil
		}
	}
	return "", fmt.Errorf("unsupported workflow condition value %q", expression)
}

func trimWorkflowParentheses(expression string) (string, error) {
	for strings.HasPrefix(expression, "(") {
		depth := 0
		quoted := false
		closesAtEnd := false
		for i := 0; i < len(expression); i++ {
			switch expression[i] {
			case '\'':
				quoted = !quoted
			case '(':
				if !quoted {
					depth++
				}
			case ')':
				if !quoted {
					depth--
					if depth < 0 {
						return "", fmt.Errorf("unbalanced workflow condition %q", expression)
					}
					if depth == 0 {
						closesAtEnd = i == len(expression)-1
						if !closesAtEnd {
							return expression, nil
						}
					}
				}
			}
		}
		if quoted || depth != 0 {
			return "", fmt.Errorf("unbalanced workflow condition %q", expression)
		}
		if !closesAtEnd {
			return expression, nil
		}
		expression = strings.TrimSpace(expression[1 : len(expression)-1])
	}
	return expression, nil
}

func splitWorkflowCondition(expression, operator string) ([]string, bool, error) {
	var parts []string
	start := 0
	depth := 0
	quoted := false
	for i := 0; i < len(expression); i++ {
		switch expression[i] {
		case '\'':
			quoted = !quoted
		case '(':
			if !quoted {
				depth++
			}
		case ')':
			if !quoted {
				depth--
				if depth < 0 {
					return nil, false, fmt.Errorf("unbalanced workflow condition %q", expression)
				}
			}
		default:
			if !quoted && depth == 0 && strings.HasPrefix(expression[i:], operator) {
				parts = append(parts, strings.TrimSpace(expression[start:i]))
				i += len(operator) - 1
				start = i + 1
			}
		}
	}
	if quoted || depth != 0 {
		return nil, false, fmt.Errorf("unbalanced workflow condition %q", expression)
	}
	if len(parts) == 0 {
		return nil, false, nil
	}
	parts = append(parts, strings.TrimSpace(expression[start:]))
	for _, part := range parts {
		if part == "" {
			return nil, false, fmt.Errorf("empty operand in workflow condition %q", expression)
		}
	}
	return parts, true, nil
}
