package service

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// SSE and WebSocket carry the same provider events. Sanitize complete logical
// output items before putting the payloads back inside their original framing.
func cleanCaptureStream(response []byte, frames []string, transport string, secrets []string) (any, bool) {
	if transport == "websocket" {
		if len(response) != 0 {
			return nil, false
		}
		return cleanCaptureFrames(frames, secrets)
	}
	if transport != "sse" || len(frames) != 0 {
		return nil, false
	}
	text := string(response)
	if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return nil, false
	}
	type line struct{ start, end, event, part int }
	var lines []line
	var events []string
	var parts []string
	var index []int
	finish := func() {
		if len(parts) == 0 {
			return
		}
		event := strings.Join(parts, "\n")
		if strings.TrimSpace(event) != "[DONE]" && strings.TrimSpace(event) != "" {
			for part, i := range index {
				lines[i].event = len(events)
				lines[i].part = part
			}
			events = append(events, event)
		}
		parts = parts[:0]
		index = index[:0]
	}
	var out strings.Builder
	last := 0
	for start := 0; start < len(text); {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		next := end + 1
		if end > start && text[end-1] == '\r' {
			end--
		}
		v := text[start:end]
		if v == "" {
			finish()
		} else if strings.HasPrefix(v, "data:") {
			offset := start + 5
			if offset < end && text[offset] == ' ' {
				offset++
			}
			index = append(index, len(lines))
			parts = append(parts, text[offset:end])
			lines = append(lines, line{offset, end, -1, 0})
		} else {
			lines = append(lines, line{start, end, -2, 0})
		}
		start = next
	}
	finish()
	cleaned, ok := cleanCaptureFrames(events, secrets)
	if !ok {
		return nil, false
	}
	var current []string
	prev := -1
	for _, l := range lines {
		v := text[l.start:l.end]
		var clean string
		switch {
		case l.event == -2:
			clean = cleanCaptureText(v, secrets)
		case l.event < 0:
			continue
		default:
			if l.event != prev {
				current = strings.Split(cleaned[l.event], "\n")
				prev = l.event
			}
			if l.part >= len(current) {
				return nil, false
			}
			clean = current[l.part]
		}
		if v != clean {
			out.WriteString(text[last:l.start])
			out.WriteString(clean)
			last = l.end
		}
	}
	if last == 0 {
		return text, true
	}
	out.WriteString(text[last:])
	return out.String(), true
}

type captureFragment struct {
	frame, start int
	raw, value   string
}
type captureStreamKey struct{ kind, item, choice, block, tool, field string }

func cleanCaptureFrames(frames []string, secrets []string) ([]string, bool) {
	groups := make(map[captureStreamKey][]captureFragment)
	for fi, frame := range frames {
		if !utf8.ValidString(frame) || !json.Valid([]byte(frame)) {
			return nil, false
		}
		r := gjson.Parse(frame)
		if !r.IsObject() {
			return nil, false
		}
		base := captureStreamKey{kind: r.Get("type").String(), item: r.Get("item_id").String(), choice: r.Get("output_index").String(), block: r.Get("content_index").String()}
		if base.item == "" {
			base.item = r.Get("call_id").String()
		}
		if base.block == "" {
			base.block = r.Get("index").String()
		}
		add := func(k captureStreamKey, field string, v gjson.Result) {
			if v.Type == gjson.String {
				k.field = field
				groups[k] = append(groups[k], captureFragment{fi, v.Index, v.Raw, v.String()})
			}
		}
		delta := r.Get("delta")
		add(base, "delta", delta)
		if delta.IsObject() {
			delta.ForEach(func(k, v gjson.Result) bool {
				if k.String() != "type" {
					add(base, k.String(), v)
				}
				return true
			})
		}
		r.Get("choices").ForEach(func(i, c gjson.Result) bool {
			k := base
			k.choice = c.Get("index").String()
			if k.choice == "" {
				k.choice = i.String()
			}
			d := c.Get("delta")
			d.ForEach(func(f, v gjson.Result) bool { add(k, f.String(), v); return true })
			add(k, "arguments", d.Get("function_call.arguments"))
			d.Get("tool_calls").ForEach(func(n, call gjson.Result) bool {
				tk := k
				tk.tool = call.Get("index").String()
				if tk.tool == "" {
					tk.tool = n.String()
				}
				add(tk, "arguments", call.Get("function.arguments"))
				add(tk, "name", call.Get("function.name"))
				return true
			})
			return true
		})
	}
	type patch struct {
		start, end int
		encoded    string
	}
	edits := make([][]patch, len(frames))
	for _, group := range groups {
		var joined strings.Builder
		for _, f := range group {
			joined.WriteString(f.value)
		}
		before := joined.String()
		clean := cleanCaptureText(before, secrets)
		if json.Valid([]byte(before)) {
			var ok bool
			clean, ok = cleanCaptureJSONOrText([]byte(before), secrets)
			if !ok {
				return nil, false
			}
		}
		if clean == before {
			continue
		}
		// Only the span between the common prefix and suffix changes frame placement.
		prefix, suffix := 0, 0
		for prefix < len(before) && prefix < len(clean) && before[prefix] == clean[prefix] {
			prefix++
		}
		for prefix > 0 && prefix < len(before) && !utf8.RuneStart(before[prefix]) {
			prefix--
		}
		for suffix < len(before)-prefix && suffix < len(clean)-prefix && before[len(before)-suffix-1] == clean[len(clean)-suffix-1] {
			suffix++
		}
		for suffix > 0 && !utf8.RuneStart(before[len(before)-suffix]) {
			suffix--
		}
		middle := clean[prefix : len(clean)-suffix]
		pos := 0
		for _, f := range group {
			end := pos + len(f.value)
			if end > prefix && pos < len(before)-suffix {
				value := f.value[:max(0, prefix-pos)] + middle + f.value[min(len(f.value), len(before)-suffix-pos):]
				middle = ""
				encoded := replaceCaptureJSONString(f.raw, f.value, value)
				edits[f.frame] = append(edits[f.frame], patch{f.start, f.start + len(f.raw), encoded})
			}
			pos = end
		}
	}
	out := make([]string, len(frames))
	for i, v := range frames {
		patches := edits[i]
		if len(patches) > 0 {
			sort.Slice(patches, func(i, j int) bool { return patches[i].start < patches[j].start })
			var b strings.Builder
			last := 0
			for _, p := range patches {
				if p.start < last || p.end > len(v) {
					return nil, false
				}
				b.WriteString(v[last:p.start])
				b.WriteString(p.encoded)
				last = p.end
			}
			b.WriteString(v[last:])
			v = b.String()
		}
		cleaned, ok := cleanCaptureJSONOrText([]byte(v), secrets)
		if !ok {
			return nil, false
		}
		out[i] = cleaned
	}
	return out, true
}
