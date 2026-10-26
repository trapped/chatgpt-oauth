package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func chatRequest(b map[string]any) (map[string]any, error) {
	if n, ok := b["n"].(float64); ok && n != 1 {
		return nil, errors.New("only n=1 is supported")
	}
	if b["stop"] != nil {
		return nil, errors.New("stop sequences are unsupported")
	}
	messages, ok := b["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("messages must be a non-empty array")
	}
	input := []any{}
	for _, v := range messages {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("invalid message")
		}
		role, _ := m["role"].(string)
		if role == "tool" {
			id, _ := m["tool_call_id"].(string)
			text, ok := m["content"].(string)
			if id == "" || !ok {
				return nil, errors.New("tool messages require tool_call_id and string content")
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": text})
			continue
		}
		if !contains([]string{"system", "developer", "user", "assistant"}, role) {
			return nil, errors.New("unsupported message role")
		}
		content, err := chatContent(m["content"], role)
		if err != nil {
			return nil, err
		}
		if len(content) > 0 {
			if role == "system" {
				role = "developer"
			}
			input = append(input, map[string]any{"role": role, "content": content})
		}
		if calls, ok := m["tool_calls"].([]any); ok {
			for _, v := range calls {
				call, ok := v.(map[string]any)
				if !ok {
					return nil, errors.New("invalid tool call")
				}
				f, ok := call["function"].(map[string]any)
				if !ok {
					return nil, errors.New("only function tool calls supported")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": call["id"], "name": f["name"], "arguments": f["arguments"]})
			}
		}
	}
	result := map[string]any{"model": b["model"], "input": input, "instructions": ""}
	for _, k := range []string{"parallel_tool_calls", "service_tier", "include", "metadata"} {
		if v := b[k]; v != nil {
			result[k] = v
		}
	}
	if v := b["reasoning_effort"]; v != nil {
		result["reasoning"] = map[string]any{"effort": v, "summary": "auto"}
	}
	if tools, ok := b["tools"].([]any); ok {
		flat := []any{}
		for _, v := range tools {
			t, ok := v.(map[string]any)
			if !ok || t["type"] != "function" {
				return nil, errors.New("only function tools supported")
			}
			f, ok := t["function"].(map[string]any)
			if !ok {
				return nil, errors.New("invalid tool definition")
			}
			tool := map[string]any{"type": "function"}
			for k, v := range f {
				tool[k] = v
			}
			flat = append(flat, tool)
		}
		result["tools"] = flat
	}
	if choice := b["tool_choice"]; choice != nil {
		if c, ok := choice.(map[string]any); ok {
			f, ok := c["function"].(map[string]any)
			if !ok {
				return nil, errors.New("invalid tool_choice")
			}
			choice = map[string]any{"type": "function", "name": f["name"]}
		}
		result["tool_choice"] = choice
	}
	if f, ok := b["response_format"].(map[string]any); ok {
		format := map[string]any{}
		switch f["type"] {
		case "json_schema":
			if schema, ok := f["json_schema"].(map[string]any); ok {
				for k, v := range schema {
					format[k] = v
				}
			}
			format["type"] = "json_schema"
		case "json_object":
			format["type"] = "json_object"
		case "text":
			format["type"] = "text"
		default:
			return nil, errors.New("unsupported response_format")
		}
		result["text"] = map[string]any{"format": format}
	}
	return result, nil
}
func chatContent(v any, role string) ([]any, error) {
	typ := "input_text"
	if role == "assistant" {
		typ = "output_text"
	}
	if v == nil {
		return nil, nil
	}
	if text, ok := v.(string); ok {
		return []any{map[string]any{"type": typ, "text": text}}, nil
	}
	parts, ok := v.([]any)
	if !ok {
		return nil, errors.New("unsupported message content")
	}
	out := []any{}
	for _, v := range parts {
		p, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New("invalid content part")
		}
		switch p["type"] {
		case "text":
			out = append(out, map[string]any{"type": typ, "text": p["text"]})
		case "image_url":
			if role != "user" {
				return nil, errors.New("images require user role")
			}
			image, ok := p["image_url"].(map[string]any)
			if !ok {
				return nil, errors.New("invalid image_url")
			}
			part := map[string]any{"type": "input_image", "image_url": image["url"]}
			if detail := image["detail"]; detail != nil {
				part["detail"] = detail
			}
			out = append(out, part)
		default:
			return nil, errors.New("unsupported content part")
		}
	}
	return out, nil
}
func usage(v any) any {
	u, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return map[string]any{"prompt_tokens": u["input_tokens"], "completion_tokens": u["output_tokens"], "total_tokens": u["total_tokens"], "prompt_tokens_details": u["input_tokens_details"], "completion_tokens_details": u["output_tokens_details"]}
}
func chatResponse(r map[string]any, model string) map[string]any {
	var text, reasoning strings.Builder
	calls := []any{}
	output, _ := r["output"].([]any)
	for _, v := range output {
		item, _ := v.(map[string]any)
		switch item["type"] {
		case "message":
			content, _ := item["content"].([]any)
			for _, v := range content {
				part, _ := v.(map[string]any)
				if t, ok := part["text"].(string); ok {
					text.WriteString(t)
				}
			}
		case "function_call":
			calls = append(calls, map[string]any{"id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": item["arguments"]}})
		case "reasoning":
			summary, _ := item["summary"].([]any)
			for _, v := range summary {
				p, _ := v.(map[string]any)
				t, _ := p["text"].(string)
				reasoning.WriteString(t)
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": nil}
	if text.Len() > 0 {
		message["content"] = text.String()
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	finish := "stop"
	if len(calls) > 0 {
		message["tool_calls"] = calls
		finish = "tool_calls"
	}
	if r["status"] == "incomplete" {
		finish = "length"
	}
	return map[string]any{"id": r["id"], "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": usage(r["usage"])}
}

type chatWriter struct {
	w                     http.ResponseWriter
	model, id             string
	created               int64
	includeUsage, started bool
	tools                 map[string]int
	arguments             map[string]bool
}

func newChatWriter(w http.ResponseWriter, model string, include bool) *chatWriter {
	return &chatWriter{w: w, model: model, id: fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), created: time.Now().Unix(), includeUsage: include, tools: map[string]int{}, arguments: map[string]bool{}}
}
func (c *chatWriter) chunk(delta any, finish any, u any) error {
	choices := []any{}
	if delta != nil || finish != nil {
		choices = append(choices, map[string]any{"index": 0, "delta": delta, "finish_reason": finish})
	}
	v := map[string]any{"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model, "choices": choices}
	if u != nil {
		v["usage"] = u
	}
	b, _ := json.Marshal(v)
	_, err := fmt.Fprintf(c.w, "data: %s\n\n", b)
	return err
}
func (c *chatWriter) event(e map[string]any) error {
	if !c.started {
		if err := c.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil); err != nil {
			return err
		}
		c.started = true
	}
	typ, _ := e["type"].(string)
	id, _ := e["item_id"].(string)
	switch typ {
	case "response.output_text.delta":
		return c.chunk(map[string]any{"content": e["delta"]}, nil, nil)
	case "response.reasoning_summary_text.delta":
		return c.chunk(map[string]any{"reasoning_content": e["delta"]}, nil, nil)
	case "response.output_item.added":
		item, _ := e["item"].(map[string]any)
		if item["type"] != "function_call" {
			return nil
		}
		id, _ = item["id"].(string)
		index := len(c.tools)
		c.tools[id] = index
		args, _ := item["arguments"].(string)
		if args != "" {
			c.arguments[id] = true
		}
		return c.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": item["call_id"], "type": "function", "function": map[string]any{"name": item["name"], "arguments": args}}}}, nil, nil)
	case "response.function_call_arguments.delta":
		index, ok := c.tools[id]
		if !ok {
			return errors.New("tool arguments before tool declaration")
		}
		c.arguments[id] = true
		return c.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": e["delta"]}}}}, nil, nil)
	case "response.output_item.done":
		item, _ := e["item"].(map[string]any)
		id, _ = item["id"].(string)
		if item["type"] == "function_call" && !c.arguments[id] {
			if index, ok := c.tools[id]; ok {
				c.arguments[id] = true
				return c.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": item["arguments"]}}}}, nil, nil)
			}
		}
	case "response.completed", "response.incomplete":
		r, _ := e["response"].(map[string]any)
		finish := "stop"
		if len(c.tools) > 0 {
			finish = "tool_calls"
		}
		if typ == "response.incomplete" {
			finish = "length"
		}
		if err := c.chunk(map[string]any{}, finish, nil); err != nil {
			return err
		}
		if c.includeUsage {
			return c.chunk(nil, nil, usage(r["usage"]))
		}
	}
	return nil
}
