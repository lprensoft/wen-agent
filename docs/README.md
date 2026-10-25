# 文档

[← 返回项目 README](../README.md)　·　中文　·　[English](en/README.md)

## 使用

- [命令行](cli.md) —— `wen serve` / `wen config` / `wen status` / `wen update` 等子命令
- [配置与模型](configuration.md) —— config.yaml 的各项、设置页里的提供商与模型管理
- [插件总览](plugins/README.md) —— 三十一个内置插件的说明与配置项
- [部署与访问控制](deployment.md) —— 远程访问的认证模型、启停脚本与 systemd
- [回放评测（wen eval）](evaluation.md) —— 把「改完提示词角色变好了没有」变成可重复跑的脚本

## 设计与开发

- [上下文的组织](context.md) —— 一轮请求怎么拼、当前时间为什么不放在 system 里
- [HTTP API](http-api.md) —— Web UI 与外部程序共用的接口
- [项目结构与插件开发](architecture.md) —— 目录导览、`Plugin` 接口与各个可选接口、可见域

## 决策记录

一次性的调研与选型结论，记录当时的判断依据，不随代码更新，也不做双语。

- [插件安装、生态兼容与语言选择](decisions/agent-plugins-analysis.md) —— 为什么继续用 Go，外源插件为什么走 Agent Plugins 标准（2026-08-25）
