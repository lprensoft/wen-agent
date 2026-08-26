package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"wen/internal/plugin"
	"wen/internal/prompt"
)

// 本文件把「单个记忆库」变成「按可见域分开的多个库」。
//
// 共享域（空标签）用基准目录，其余域用同级的 <base>-<tag>，见 plugin.DomainDir。
// 写入只落在本轮的写入域，读取合并本轮所有可读域——不可读域的记忆不仅内容读不到，
// 连标题与条数也不该露出来，否则「存在什么」本身就是泄漏。

// storeFor 返回某个可见域的记忆库（惰性创建）。未初始化时返回 nil。
func (p *Plugin) storeFor(tag string) *Store {
	p.mu.RLock()
	base, lang := p.libBase, p.lang
	baseStore := p.store
	p.mu.RUnlock()
	if base == "" || baseStore == nil {
		return nil
	}
	if tag == "" {
		return baseStore
	}

	p.storesMu.Lock()
	defer p.storesMu.Unlock()
	if p.stores == nil {
		p.stores = map[string]*Store{}
	}
	if s, ok := p.stores[tag]; ok {
		return s
	}
	s := NewStore(lang, plugin.DomainDir(base, tag))
	p.stores[tag] = s
	return s
}

// writeStore 返回本轮该写入的库。
func (p *Plugin) writeStore(ctx context.Context) *Store {
	return p.storeFor(plugin.ScopeFrom(ctx).Write)
}

// readDomains 返回本轮可读的可见域，写入域在最前。
func (p *Plugin) readDomains(ctx context.Context) []string {
	p.mu.RLock()
	base := p.libBase
	p.mu.RUnlock()
	return plugin.ReadDomains(base, plugin.ScopeFrom(ctx))
}

// visibleEntries 合并本轮所有可读域的记忆，按（分类, 标题）排序。
// 同名记忆只保留一条，取自靠前的域（写入域优先）：模型看到的是一份没有重影的清单，
// recall 与 delete 也就落在同一条上。
func (p *Plugin) visibleEntries(ctx context.Context) ([]Entry, error) {
	var (
		out  []Entry
		seen = map[string]bool{}
		errs []string
	)
	for _, tag := range p.readDomains(ctx) {
		s := p.storeFor(tag)
		if s == nil {
			continue
		}
		entries, err := s.List()
		if err != nil {
			errs = append(errs, err.Error())
			continue // 单个域读不出来不该让其余域也用不了
		}
		for _, e := range entries {
			key := strings.ToLower(e.Name)
			if seen[key] {
				continue
			}
			seen[key] = true
			e.Domain = tag
			out = append(out, e)
		}
	}
	if out == nil && len(errs) > 0 {
		return nil, fmt.Errorf(prompt.T(p.langOf(), "prompt.memory.err.read_domains",
			"读取记忆失败: %s"), strings.Join(errs, "; "))
	}
	sortEntries(out)
	return out, nil
}

// findVisible 在本轮可读的记忆里按标题查找，并给出它所属的库。
func (p *Plugin) findVisible(ctx context.Context, name string) (Entry, *Store, error) {
	entries, err := p.visibleEntries(ctx)
	if err != nil {
		return Entry{}, nil, err
	}
	e, ok := findEntry(entries, name)
	if !ok {
		return Entry{}, nil, fmt.Errorf(prompt.T(p.langOf(), "prompt.memory.err.not_found",
			"没有名为 %q 的记忆"), name)
	}
	return e, p.storeFor(e.Domain), nil
}

// sortEntries 按（分类展示序, 标题）稳定排序。跨库合并后要重新排一次，
// 否则索引会按库分块，读起来像是同一批记忆被切成了几段。
func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := typeRank(entries[i].Type), typeRank(entries[j].Type)
		if ri != rj {
			return ri < rj
		}
		return entries[i].Name < entries[j].Name
	})
}
