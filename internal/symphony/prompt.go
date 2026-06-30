package symphony

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

func RenderPrompt(template string, issue Issue, attempt *int) (string, error) {
	if strings.TrimSpace(template) == "" {
		template = DefaultPrompt
	}
	vars := map[string]any{
		"issue":   issueToMap(issue),
		"attempt": nil,
	}
	if attempt != nil {
		vars["attempt"] = *attempt
	}
	out, err := renderLiquidSection(template, vars)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func issueToMap(issue Issue) map[string]any {
	m := map[string]any{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"description": nil,
		"priority":    nil,
		"state":       issue.State,
		"branch_name": nil,
		"url":         nil,
		"labels":      append([]string(nil), issue.Labels...),
		"blocked_by":  []map[string]any{},
		"created_at":  nil,
		"updated_at":  nil,
	}
	if issue.Description != nil {
		m["description"] = *issue.Description
	}
	if issue.Priority != nil {
		m["priority"] = *issue.Priority
	}
	if issue.BranchName != nil {
		m["branch_name"] = *issue.BranchName
	}
	if issue.URL != nil {
		m["url"] = *issue.URL
	}
	if issue.CreatedAt != nil {
		m["created_at"] = issue.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if issue.UpdatedAt != nil {
		m["updated_at"] = issue.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	blockers := make([]map[string]any, 0, len(issue.BlockedBy))
	for _, b := range issue.BlockedBy {
		row := map[string]any{"id": nil, "identifier": nil, "state": nil}
		if b.ID != nil {
			row["id"] = *b.ID
		}
		if b.Identifier != nil {
			row["identifier"] = *b.Identifier
		}
		if b.State != nil {
			row["state"] = *b.State
		}
		blockers = append(blockers, row)
	}
	m["blocked_by"] = blockers
	return m
}

func renderLiquidSection(input string, vars map[string]any) (string, error) {
	var b strings.Builder
	for len(input) > 0 {
		nextExpr := strings.Index(input, "{{")
		nextTag := strings.Index(input, "{%")
		if nextExpr == -1 && nextTag == -1 {
			b.WriteString(input)
			break
		}
		isExpr := false
		next := nextTag
		if nextExpr != -1 && (nextTag == -1 || nextExpr < nextTag) {
			next = nextExpr
			isExpr = true
		}
		b.WriteString(input[:next])
		input = input[next:]
		if isExpr {
			end := strings.Index(input, "}}")
			if end == -1 {
				return "", fmt.Errorf("%w: unterminated interpolation", ErrTemplateParse)
			}
			expr := strings.TrimSpace(input[2:end])
			value, err := evalLiquidExpression(expr, vars)
			if err != nil {
				return "", err
			}
			b.WriteString(stringifyTemplateValue(value))
			input = input[end+2:]
			continue
		}

		end := strings.Index(input, "%}")
		if end == -1 {
			return "", fmt.Errorf("%w: unterminated tag", ErrTemplateParse)
		}
		tag := strings.TrimSpace(input[2:end])
		rest := input[end+2:]
		switch {
		case strings.HasPrefix(tag, "for "):
			varName, listExpr, err := parseForTag(tag)
			if err != nil {
				return "", err
			}
			body, after, err := splitBlock(rest, "for", "endfor", "")
			if err != nil {
				return "", err
			}
			list, err := evalLiquidExpression(listExpr, vars)
			if err != nil {
				return "", err
			}
			items, err := iterableValues(list)
			if err != nil {
				return "", err
			}
			for _, item := range items {
				child := copyVars(vars)
				child[varName] = item
				rendered, err := renderLiquidSection(body, child)
				if err != nil {
					return "", err
				}
				b.WriteString(rendered)
			}
			input = after
		case strings.HasPrefix(tag, "if "):
			condition := strings.TrimSpace(strings.TrimPrefix(tag, "if "))
			trueBody, falseBody, after, err := splitIfBlock(rest)
			if err != nil {
				return "", err
			}
			ok, err := evalCondition(condition, vars)
			if err != nil {
				return "", err
			}
			body := falseBody
			if ok {
				body = trueBody
			}
			rendered, err := renderLiquidSection(body, vars)
			if err != nil {
				return "", err
			}
			b.WriteString(rendered)
			input = after
		case strings.HasPrefix(tag, "comment"):
			_, after, err := splitBlock(rest, "comment", "endcomment", "")
			if err != nil {
				return "", err
			}
			input = after
		case tag == "":
			input = rest
		default:
			return "", fmt.Errorf("%w: unsupported tag %q", ErrTemplateParse, tag)
		}
	}
	return b.String(), nil
}

func parseForTag(tag string) (string, string, error) {
	body := strings.TrimSpace(strings.TrimPrefix(tag, "for "))
	parts := strings.SplitN(body, " in ", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("%w: invalid for tag", ErrTemplateParse)
	}
	name := strings.TrimSpace(parts[0])
	if name == "" || strings.ContainsAny(name, ".| ") {
		return "", "", fmt.Errorf("%w: invalid for variable", ErrTemplateParse)
	}
	return name, strings.TrimSpace(parts[1]), nil
}

func splitBlock(input, opener, closer, alternate string) (string, string, error) {
	depth := 1
	pos := 0
	for {
		idx := strings.Index(input[pos:], "{%")
		if idx == -1 {
			return "", "", fmt.Errorf("%w: missing %s", ErrTemplateParse, closer)
		}
		idx += pos
		end := strings.Index(input[idx:], "%}")
		if end == -1 {
			return "", "", fmt.Errorf("%w: unterminated tag", ErrTemplateParse)
		}
		end += idx
		tag := strings.TrimSpace(input[idx+2 : end])
		switch {
		case strings.HasPrefix(tag, opener+" "):
			depth++
		case tag == closer:
			depth--
			if depth == 0 {
				return input[:idx], input[end+2:], nil
			}
		case alternate != "" && tag == alternate && depth == 1:
			return input[:idx], input[end+2:], nil
		}
		pos = end + 2
	}
}

func splitIfBlock(input string) (string, string, string, error) {
	depth := 1
	pos := 0
	elseStart := -1
	elseEnd := -1
	for {
		idx := strings.Index(input[pos:], "{%")
		if idx == -1 {
			return "", "", "", fmt.Errorf("%w: missing endif", ErrTemplateParse)
		}
		idx += pos
		end := strings.Index(input[idx:], "%}")
		if end == -1 {
			return "", "", "", fmt.Errorf("%w: unterminated tag", ErrTemplateParse)
		}
		end += idx
		tag := strings.TrimSpace(input[idx+2 : end])
		switch {
		case strings.HasPrefix(tag, "if "):
			depth++
		case tag == "endif":
			depth--
			if depth == 0 {
				trueBody := input[:idx]
				falseBody := ""
				if elseStart != -1 {
					trueBody = input[:elseStart]
					falseBody = input[elseEnd:idx]
				}
				return trueBody, falseBody, input[end+2:], nil
			}
		case tag == "else" && depth == 1:
			elseStart = idx
			elseEnd = end + 2
		}
		pos = end + 2
	}
}

func evalLiquidExpression(expr string, vars map[string]any) (any, error) {
	parts := strings.Split(expr, "|")
	valueExpr := strings.TrimSpace(parts[0])
	if valueExpr == "" {
		return "", fmt.Errorf("%w: empty expression", ErrTemplateRender)
	}
	value, err := evalValue(valueExpr, vars)
	if err != nil {
		return nil, err
	}
	for _, filter := range parts[1:] {
		name, argText := parseFilter(strings.TrimSpace(filter))
		switch name {
		case "json":
			data, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("%w: json filter failed: %v", ErrTemplateRender, err)
			}
			value = string(data)
		case "join":
			sep := ""
			if strings.TrimSpace(argText) != "" {
				arg, err := evalValue(strings.TrimSpace(argText), vars)
				if err != nil {
					return nil, err
				}
				sep = stringifyTemplateValue(arg)
			}
			items, err := iterableValues(value)
			if err != nil {
				return nil, err
			}
			parts := make([]string, 0, len(items))
			for _, item := range items {
				parts = append(parts, stringifyTemplateValue(item))
			}
			value = strings.Join(parts, sep)
		case "default":
			if truthy(value) {
				continue
			}
			if strings.TrimSpace(argText) == "" {
				return nil, fmt.Errorf("%w: default filter requires an argument", ErrTemplateRender)
			}
			value, err = evalValue(strings.TrimSpace(argText), vars)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: unknown filter %q", ErrTemplateRender, name)
		}
	}
	return value, nil
}

func parseFilter(filter string) (string, string) {
	parts := strings.SplitN(filter, ":", 2)
	name := strings.TrimSpace(parts[0])
	if len(parts) == 1 {
		return name, ""
	}
	return name, strings.TrimSpace(parts[1])
}

func evalCondition(expr string, vars map[string]any) (bool, error) {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "not ") {
		ok, err := evalCondition(strings.TrimSpace(strings.TrimPrefix(expr, "not ")), vars)
		return !ok, err
	}
	for _, op := range []string{"==", "!="} {
		if idx := strings.Index(expr, op); idx != -1 {
			left, err := evalValue(strings.TrimSpace(expr[:idx]), vars)
			if err != nil {
				return false, err
			}
			right, err := evalValue(strings.TrimSpace(expr[idx+len(op):]), vars)
			if err != nil {
				return false, err
			}
			eq := stringifyTemplateValue(left) == stringifyTemplateValue(right)
			if op == "!=" {
				return !eq, nil
			}
			return eq, nil
		}
	}
	value, err := evalLiquidExpression(expr, vars)
	if err != nil {
		return false, err
	}
	return truthy(value), nil
}

func evalValue(expr string, vars map[string]any) (any, error) {
	expr = strings.TrimSpace(expr)
	if expr == "nil" || expr == "null" {
		return nil, nil
	}
	if expr == "true" {
		return true, nil
	}
	if expr == "false" {
		return false, nil
	}
	if len(expr) >= 2 && ((expr[0] == '"' && expr[len(expr)-1] == '"') || (expr[0] == '\'' && expr[len(expr)-1] == '\'')) {
		unquoted, err := strconv.Unquote(expr)
		if err != nil && expr[0] == '\'' {
			return expr[1 : len(expr)-1], nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: invalid string literal", ErrTemplateRender)
		}
		return unquoted, nil
	}
	if n, err := strconv.Atoi(expr); err == nil {
		return n, nil
	}
	return lookupVar(expr, vars)
}

func lookupVar(path string, vars map[string]any) (any, error) {
	parts := strings.Split(path, ".")
	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("%w: unknown variable %q", ErrTemplateRender, path)
	}
	value, ok := vars[parts[0]]
	if !ok {
		return nil, fmt.Errorf("%w: unknown variable %q", ErrTemplateRender, parts[0])
	}
	for _, part := range parts[1:] {
		if part == "" {
			return nil, fmt.Errorf("%w: invalid variable path %q", ErrTemplateRender, path)
		}
		switch x := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = x[part]
			if !ok {
				return nil, fmt.Errorf("%w: unknown variable %q", ErrTemplateRender, path)
			}
		case map[string]string:
			var ok bool
			value, ok = x[part]
			if !ok {
				return nil, fmt.Errorf("%w: unknown variable %q", ErrTemplateRender, path)
			}
		default:
			rv := reflect.ValueOf(value)
			if rv.Kind() == reflect.Pointer {
				if rv.IsNil() {
					return nil, fmt.Errorf("%w: unknown variable %q", ErrTemplateRender, path)
				}
				rv = rv.Elem()
			}
			if rv.Kind() == reflect.Struct {
				field := rv.FieldByNameFunc(func(name string) bool {
					return strings.EqualFold(name, part) || strings.EqualFold(toSnake(name), part)
				})
				if field.IsValid() && field.CanInterface() {
					value = field.Interface()
					continue
				}
			}
			return nil, fmt.Errorf("%w: unknown variable %q", ErrTemplateRender, path)
		}
	}
	return value, nil
}

func iterableValues(value any) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	rv := reflect.ValueOf(value)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out = append(out, rv.Index(i).Interface())
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: value is not iterable", ErrTemplateRender)
	}
}

func stringifyTemplateValue(value any) string {
	if value == nil {
		return ""
	}
	switch x := value.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

func truthy(value any) bool {
	if value == nil {
		return false
	}
	switch x := value.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	default:
		rv := reflect.ValueOf(value)
		switch rv.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			return rv.Len() > 0
		}
		return true
	}
}

func copyVars(vars map[string]any) map[string]any {
	out := make(map[string]any, len(vars)+1)
	for k, v := range vars {
		out[k] = v
	}
	return out
}

func toSnake(name string) string {
	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}
