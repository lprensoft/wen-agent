package plugin

import (
	"fmt"

	"wen/internal/prompt"
)

// BadArgs 是「模型给的参数解不开」这一条错误，四十来处工具逐字写着同一句。
//
// 抽出来是因为它要跟随语言了：一句话四十份译文，改一个字就得改四十处，而漏掉的那几处
// 在英文界面上会以中文出现，谁都不会注意到。这里也顺手统一了措辞——原先有「参数格式
// 错误」「参数解析失败」几种写法，模型看到的是同一件事的三种说法。
func BadArgs(lang string, err error) error {
	return fmt.Errorf(prompt.T(lang, "prompt.tool.bad_args", "参数格式错误: %w"), err)
}
