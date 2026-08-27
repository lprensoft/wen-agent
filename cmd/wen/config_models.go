package main

import (
	"errors"
	"strings"

	"github.com/charmbracelet/huh"

	"wen/internal/modelcfg"
)

// modelsSection 是模型配置：切换当前模型、增删改提供商。
func modelsSection(b backend) error {
	for {
		doc, err := b.loadModels()
		if err != nil {
			return err
		}

		opts := []huh.Option[string]{
			huh.NewOption(tr("cli.models.switch", "切换当前模型"), "current"),
			huh.NewOption(tr("cli.models.addProvider", "新增提供商"), "add"),
		}
		for _, p := range doc.Providers {
			opts = append(opts, huh.NewOption(
				trf("cli.models.editProvider", "编辑 %s（%s，%d 个模型%s）",
					p.Name, p.Type, len(p.Models), keyHint(p)),
				"edit:"+p.Name))
		}
		opts = append(opts, huh.NewOption(tr("cli.back", "← 返回"), back))
		for i, o := range opts {
			opts[i] = huh.NewOption(fit(o.Key), o.Value)
		}

		choice := "current" // 光标停在第一项，见 topMenu 的说明
		if err := run(huh.NewSelect[string]().
			Title(tr("cli.models.title", "模型")).
			Description(fitLines(trf("cli.models.current", "当前：%s / %s\n%s",
								doc.Current.Provider, doc.Current.Model, b.mode()))).
			Height(listHeight(len(opts), 3)). // chrome：标题 1 行 + 说明 2 行
			Options(opts...).
			Value(&choice)); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}

		var opErr error
		switch {
		case choice == back:
			return nil
		case choice == "current":
			opErr = switchCurrent(b, doc)
		case choice == "add":
			opErr = addProvider(b, doc)
		case strings.HasPrefix(choice, "edit:"):
			name := strings.TrimPrefix(choice, "edit:")
			for _, p := range doc.Providers {
				if p.Name == name {
					opErr = editProvider(b, doc, p, "", false)
					break
				}
			}
		}
		if opErr != nil && !errors.Is(opErr, huh.ErrUserAborted) {
			note("× %v", opErr)
		}
	}
}

func keyHint(p providerView) string {
	if p.HasAPIKey {
		return trf("cli.models.keySet", "，密钥 %s", p.MaskedKey)
	}
	return tr("cli.models.keyUnset", "，未配置密钥")
}

// switchCurrent 两步选：先提供商，再该提供商下的模型。
func switchCurrent(b backend, doc modelsDoc) error {
	provOpts := make([]huh.Option[string], 0, len(doc.Providers))
	for _, p := range doc.Providers {
		provOpts = append(provOpts, huh.NewOption(p.Name, p.Name))
	}
	if len(provOpts) == 0 {
		return errors.New(tr("cli.models.noProvider", "还没有配置任何提供商"))
	}
	provider := doc.Current.Provider
	if err := run(huh.NewSelect[string]().Title(tr("cli.models.provider", "提供商")).
		Options(provOpts...).Value(&provider)); err != nil {
		return err
	}

	var models []modelcfg.Model
	for _, p := range doc.Providers {
		if p.Name == provider {
			models = p.Models
		}
	}
	if len(models) == 0 {
		return errors.New(trf("cli.models.noModel", "提供商 %s 下还没有模型，请先在「编辑 %s」里添加", provider, provider))
	}
	modelOpts := make([]huh.Option[string], 0, len(models))
	for _, m := range models {
		label := m.ID
		if m.Name != "" {
			label = trf("cli.models.modelLabel", "%s（%s）", m.Name, m.ID)
		}
		modelOpts = append(modelOpts, huh.NewOption(label, m.ID))
	}
	model := doc.Current.Model
	if err := run(huh.NewSelect[string]().Title(tr("cli.models.title", "模型")).
		Options(modelOpts...).Value(&model)); err != nil {
		return err
	}

	if err := b.setCurrentModel(modelcfg.Selection{Provider: provider, Model: model}); err != nil {
		return err
	}
	note(trf("cli.models.switched", "✓ 当前模型已切换为 %s / %s", provider, model))
	return nil
}

// editProvider 编辑（或新增）一个提供商。
//
// 模型列表用多行文本编辑，一行一个，格式 `模型id` 或 `模型id | 显示名`。
// 单个模型上的参数覆盖（context_length、thinking 等）在这里改不了，但会原样保留；
// 只有把某行删掉时，那条覆盖才随之消失。需要逐项调参就去 Web UI——
// 在终端里铺开每个模型的六个可选覆盖，翻找的代价大于收益。
// addProvider 是「新增提供商」那一步：先从预置表里挑一家，挑不到才手工填。
// 预置带来的地址、方言与模型清单都是填好的，用户通常只需要补一个密钥。
func addProvider(b backend, doc modelsDoc) error {
	opts := make([]huh.Option[string], 0, len(doc.Presets)+2)
	for _, p := range doc.Presets {
		opts = append(opts, huh.NewOption(fit(p.Name+" — "+p.Description), p.Key))
	}
	// 「自定义」压在最后：它不是第十九家提供商，是另一条路
	opts = append(opts,
		huh.NewOption(fit(tr("cli.models.presetCustom", "自定义 — 上面没有你要的那家，手工填写")), "custom"),
		huh.NewOption(tr("cli.back", "← 返回"), back))

	choice := ""
	if len(opts) > 0 {
		choice = opts[0].Value // 光标停在第一项，见 topMenu 的说明
	}
	if err := run(huh.NewSelect[string]().
		Title(tr("cli.models.presetTitle", "选择提供商")).
		Description(fitLines(tr("cli.models.presetDesc", "挑一家即可，地址与常用模型都已填好，你只要补上 API Key。"))).
		Height(listHeight(len(opts), 3)). // chrome：标题 1 行 + 说明 2 行
		Options(opts...).
		Value(&choice)); err != nil {
		return err
	}
	switch choice {
	case back:
		return nil
	case "custom":
		return editProvider(b, doc, providerView{Type: "openai_compat"}, "", true)
	}
	for _, p := range doc.Presets {
		if p.Key == choice {
			return editProvider(b, doc, providerView{
				Name: p.Name, Type: p.Type, BaseURL: p.BaseURL,
				Dialect: p.Dialect, Models: p.Models,
			}, p.ConsoleURL, true)
		}
	}
	return nil
}

// consoleURL 非空时接在 API Key 那一栏的说明后面 —— 「去哪儿拿密钥」正是新手最容易卡住的一步。
func editProvider(b backend, doc modelsDoc, p providerView, consoleURL string, isNew bool) error {
	name := p.Name
	typ := p.Type
	baseURL := p.BaseURL
	dialect := p.Dialect
	newKey := ""
	modelsText := modelsToText(p.Models)
	cache := cacheToString(p.PromptCache)
	remove := false

	keyDesc := tr("cli.models.keyKeep", "留空表示不修改")
	if !p.HasAPIKey {
		keyDesc = tr("cli.models.keyNone", "尚未配置")
	}
	if consoleURL != "" {
		keyDesc = trf("cli.models.keyFrom", "%s，去这里拿：%s", keyDesc, consoleURL)
	}

	typeOpts := make([]huh.Option[string], 0)
	for _, t := range modelcfg.TypeOptions(uiLang) {
		typeOpts = append(typeOpts, huh.NewOption(t["label"], t["value"]))
	}
	dialectOpts := make([]huh.Option[string], 0)
	for _, d := range modelcfg.DialectOptions(uiLang) {
		dialectOpts = append(dialectOpts, huh.NewOption(d["label"], d["value"]))
	}

	fields := []huh.Field{
		huh.NewInput().Title(tr("cli.models.name", "名称")).Description(tr("cli.models.nameDesc", "唯一标识，也是界面上的显示名")).Value(&name).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New(tr("cli.models.nameEmpty", "名称不能为空"))
				}
				return nil
			}),
		huh.NewSelect[string]().Title(tr("cli.models.apiType", "接口类型")).Options(typeOpts...).Value(&typ),
		huh.NewInput().Title("Base URL").Value(&baseURL),
		huh.NewInput().Title("API Key").Description(keyDesc).
			EchoMode(huh.EchoModePassword).Value(&newKey),
		huh.NewSelect[string]().Title(tr("cli.models.dialect", "思考参数方言")).
			Description(tr("cli.models.dialectDesc", "仅 openai_compat 生效")).Options(dialectOpts...).Value(&dialect),
		huh.NewSelect[string]().Title(tr("cli.models.cache", "提示词缓存")).
			Description(tr("cli.models.cacheDesc", "仅 anthropic 生效。命中约十分之一价，未命中的写入多付约四分之一；\n对话间隔常超过几分钟时关掉更省。")).
			Options(
				huh.NewOption(tr("cli.models.cacheDefault", "跟随默认（开启）"), ""),
				huh.NewOption(tr("cli.models.cacheOn", "开启"), "on"),
				huh.NewOption(tr("cli.models.cacheOff", "关闭"), "off"),
			).Value(&cache),
		huh.NewText().Title(tr("cli.models.list", "模型列表")).
			Description(tr("cli.models.listDesc", "一行一个，格式：模型id  或  模型id | 显示名")).
			Lines(8).Value(&modelsText),
	}
	if !isNew {
		fields = append(fields, huh.NewConfirm().
			Title(tr("cli.models.deleteTitle", "删除这个提供商")).Affirmative(tr("cli.delete", "删除")).Negative(tr("cli.keep", "保留")).Value(&remove))
	}

	if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
		return err
	}

	next := doc.toFile()
	if remove {
		next.Providers = dropProvider(next.Providers, p.Name)
		if err := b.saveModels(next); err != nil {
			return err
		}
		note(trf("cli.models.deleted", "✓ 提供商 %s 已删除", p.Name))
		return nil
	}

	edited := modelcfg.Provider{
		Name: strings.TrimSpace(name), Type: typ, BaseURL: strings.TrimSpace(baseURL),
		APIKey:  newKey, // 留空 = 沿用旧值，由服务端的 carryOverKeys 补上
		Dialect: dialect, PromptCache: stringToCache(cache),
		Models: textToModels(modelsText, p.Models),
	}
	if isNew {
		next.Providers = append(next.Providers, edited)
	} else {
		next.Providers = replaceProvider(next.Providers, p.Name, edited)
	}
	if err := b.saveModels(next); err != nil {
		return err
	}
	note(trf("cli.models.savedProvider", "✓ 提供商 %s 已保存", edited.Name))
	return nil
}

func dropProvider(list []modelcfg.Provider, name string) []modelcfg.Provider {
	out := make([]modelcfg.Provider, 0, len(list))
	for _, p := range list {
		if p.Name != name {
			out = append(out, p)
		}
	}
	return out
}

func replaceProvider(list []modelcfg.Provider, name string, next modelcfg.Provider) []modelcfg.Provider {
	for i, p := range list {
		if p.Name == name {
			list[i] = next
			return list
		}
	}
	return append(list, next)
}

func modelsToText(models []modelcfg.Model) string {
	lines := make([]string, 0, len(models))
	for _, m := range models {
		if m.Name != "" {
			lines = append(lines, m.ID+" | "+m.Name)
			continue
		}
		lines = append(lines, m.ID)
	}
	return strings.Join(lines, "\n")
}

// textToModels 解析模型列表。已存在的 id 保留其参数覆盖——那些字段在这一屏改不了，
// 若不带过来，一次无关的编辑就会把它们全清掉。
func textToModels(text string, prev []modelcfg.Model) []modelcfg.Model {
	byID := map[string]modelcfg.Model{}
	for _, m := range prev {
		byID[m.ID] = m
	}
	out := []modelcfg.Model{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, display, _ := strings.Cut(line, "|")
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		m := byID[id]
		m.ID = id
		m.Name = strings.TrimSpace(display)
		out = append(out, m)
	}
	return out
}

// 提示词缓存是三态：未设置 / 明确开 / 明确关。未设置要原样写回 null，
// 否则一次保存就把条目从 config.yaml 接管了过来。
func cacheToString(p *bool) string {
	switch {
	case p == nil:
		return ""
	case *p:
		return "on"
	default:
		return "off"
	}
}

func stringToCache(s string) *bool {
	switch s {
	case "on":
		v := true
		return &v
	case "off":
		v := false
		return &v
	default:
		return nil
	}
}
