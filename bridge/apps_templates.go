package main

import (
	_ "embed"
	"encoding/json"
	"maps"
	"slices"
)

//go:embed apps_templates.js
var appTemplatesJS string

type appTemplate struct {
	title  string
	fields object
}

func closedObject(props object, required ...string) object {
	if required == nil {
		required = []string{}
	}
	return object{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

var templateCatalog = func() map[string]appTemplate {
	text := object{"type": "string"}
	num := object{"type": "integer"}
	array := func(item object) object { return object{"type": "array", "items": item} }
	shape := object{"type": "string", "enum": []string{"star", "circle", "diamond", "square", "triangle", "heart"}}
	return map[string]appTemplate{
		"reader":              {"Story", object{"text": text, "pages": array(object{"anyOf": []object{text, closedObject(object{"title": text, "text": text}, "text")}}), "fontSize": num}},
		"quiz":                {"Quiz", object{"questions": array(closedObject(object{"prompt": text, "choices": array(text), "answer": num}, "prompt", "choices", "answer"))}},
		"spot-the-difference": {"Spot the Difference", object{"tiles": array(closedObject(object{"left": shape, "right": shape}, "left", "right"))}},
		"drawing":             {"Drawing", object{"colors": array(text)}},
		"pet":                 {"My Pet", object{"name": text, "pet_color": text}},
		"timer":               {"Countdown", object{"seconds": num}},
		"picture-math":        {"Picture Math", object{"problems": array(closedObject(object{"left": num, "right": num}, "left", "right"))}},
		"memory-match":        {"Memory Match", object{"cards": array(text)}},
		"fireworks":           {"Fireworks", object{"colors": array(text)}},
	}
}()

var templateCommon = object{"title": object{"type": "string"}, "theme": object{"type": "string", "enum": []string{"warm", "night", "mint", "sky"}}}

func appTemplateSchema(name string) object {
	fields := maps.Clone(templateCommon)
	if name != "" {
		maps.Copy(fields, templateCatalog[name].fields)
	} else {
		for _, t := range templateCatalog {
			maps.Copy(fields, t.fields)
		}
	}
	return closedObject(fields)
}

func templateNames() []string { return slices.Sorted(maps.Keys(templateCatalog)) }

func validateTemplateValue(v any, schema object, path string) error {
	if branches, ok := schema["anyOf"].([]object); ok {
		for _, branch := range branches {
			if validateTemplateValue(v, branch, path) == nil {
				return nil
			}
		}
		return fieldError(path, "invalid value")
	}
	switch schema["type"] {
	case "object":
		if o, ok := v.(object); ok {
			props := schema["properties"].(object)
			for _, key := range slices.Sorted(maps.Keys(o)) {
				child, ok := props[key].(object)
				if !ok {
					return fieldError(path+"."+key, "unknown argument")
				}
				if err := validateTemplateValue(o[key], child, path+"."+key); err != nil {
					return err
				}
			}
			for _, key := range schema["required"].([]string) {
				if _, ok := o[key]; !ok {
					return fieldError(path+"."+key, "required")
				}
			}
			return nil
		}
	case "array":
		if list, ok := v.([]any); ok {
			for _, item := range list {
				if err := validateTemplateValue(item, schema["items"].(object), path+"[]"); err != nil {
					return err
				}
			}
			return nil
		}
	case "string":
		if text, ok := v.(string); ok {
			if values, ok := schema["enum"].([]string); ok && !slices.Contains(values, text) {
				return fieldError(path, "expected one of %v", values)
			}
			return nil
		}
	case "integer":
		if n, ok := v.(json.Number); ok {
			if _, err := n.Int64(); err == nil {
				return nil
			}
		}
	}
	return fieldError(path, "expected %s", schema["type"])
}

func (a *savedApp) setTemplate(name string, raw any) error {
	t, ok := templateCatalog[name]
	if !ok {
		return fieldError("template", "expected one of %v", templateNames())
	}
	if raw == nil {
		raw = object{}
	}
	if err := validateTemplateValue(raw, appTemplateSchema(name), "data"); err != nil {
		return err
	}
	data, err := appJSONObject(raw, "data")
	if err != nil {
		return err
	}
	a.Template, a.Data = name, data
	if a.Title == "" {
		a.Title = shortText(raw.(object)["title"], 60, false)
		if a.Title == "" {
			a.Title = t.title
		}
	}
	if name == "timer" {
		a.TickMS, a.MaxTicks = 1000, 240
	}
	return nil
}

func (a *appInstance) installTemplate() (string, error) {
	if _, ok := templateCatalog[a.spec.Template]; !ok {
		return "", fieldError("template", "unknown template %q", a.spec.Template)
	}
	if raw := a.memory["content"]; len(raw) > 0 {
		content, err := decodeObject(raw)
		if err != nil {
			return "", fieldError("app memory content", "expected an object")
		}
		if err := validateTemplateValue(content, appTemplateSchema(a.spec.Template), "app memory content"); err != nil {
			return "", err
		}
	}
	v, err := a.call(a.parse, a.vm.ToValue(a.spec.dataJSON()))
	if err != nil {
		return "", err
	}
	_ = a.vm.Set("templateData", v)
	_ = a.vm.Set("templateName", a.spec.Template)
	_ = a.vm.Set("templateTitle", a.spec.Title)
	palettes := make(map[string][]string, 4)
	for _, theme := range []string{"warm", "night", "mint", "sky"} {
		p := uiColors(theme)
		palettes[theme] = []string{p.bg, p.card, p.ink, p.accent, p.border}
	}
	_ = a.vm.Set("templatePalettes", palettes)
	_ = a.vm.Set("templateRadii", []int{uiButtonRadius, uiCardRadius})
	return appTemplatesJS, nil
}
