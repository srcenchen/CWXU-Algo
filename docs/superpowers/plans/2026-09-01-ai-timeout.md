# AI 超时统一实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将所有后端 LLM 链路统一为 10 分钟，并要求模型快速完成、题面分类允许近似判断。

**架构：** 在公共 OpenAI 客户端包导出唯一的 `LLMCallTimeout` 常量，题库和 agent 模型客户端都使用它。各 AI 业务任务的外层 context 和锁同步覆盖 10 分钟，避免外层先取消；提示词只增加速度约束，不改变输出契约和错误回退。

**技术栈：** Go、openai-go v3、context、Redis 锁、RabbitMQ、Go testing

---

### 任务 1：公共模型超时

**文件：**
- 修改：`app/common/openaiclient/client.go`
- 创建：`app/common/openaiclient/client_test.go`

- [ ] **步骤 1：编写失败测试**

在 `client_test.go` 中断言 `LLMCallTimeout == 10*time.Minute`。

- [ ] **步骤 2：运行测试确认失败**

运行：`go test ./app/common/openaiclient`
预期：FAIL，`LLMCallTimeout` 未定义。

- [ ] **步骤 3：添加公共常量**

在 `client.go` 中添加：

```go
const LLMCallTimeout = 10 * time.Minute
```

- [ ] **步骤 4：运行测试确认通过**

运行：`go test ./app/common/openaiclient`
预期：PASS。

### 任务 2：题库 AI 链路与速度提示

**文件：**
- 修改：`app/core_data/internal/biz/service/problem_tagger.go`
- 修改：`app/core_data/internal/biz/service/problem_consumer.go`
- 测试：`app/core_data/internal/biz/service/problem_edit_test.go`

- [ ] **步骤 1：添加提示词失败测试**

新增测试，断言题库系统提示包含“优先快速完成”“大致判断”“完整翻译不得省略”三个语义短语，并将系统提示提取为可测试函数 `problemAnalyzeSystemPrompt()`。

- [ ] **步骤 2：运行测试确认失败**

运行：`go test ./app/core_data/internal/biz/service -run 'TestProblemAnalyzePrompt'`
预期：FAIL，缺少速度优先约束。

- [ ] **步骤 3：实现题库改动**

让 `ProblemTagger` 客户端使用 `openaiclient.LLMCallTimeout`；consumer 使用相同常量创建 `ProcessAnalyze` context；提示词明确分类允许近似、翻译仍必须完整。

- [ ] **步骤 4：运行题库测试**

运行：`go test ./app/core_data/internal/biz/service`
预期：PASS。

### 任务 3：日报、周报和训练报告链路

**文件：**
- 修改：`app/agent/internal/agent/chat.go`
- 修改：`app/agent/internal/biz/service/summary.go`
- 修改：`app/agent/internal/biz/service/summary_prompt.go`
- 修改：`app/agent/internal/biz/service/training_report.go`
- 修改：`app/agent/internal/biz/service/training_report_template.go`
- 测试：`app/agent/internal/agent/chat_test.go`
- 测试：`app/agent/internal/biz/service/training_report_test.go`

- [ ] **步骤 1：添加提示词失败测试**

断言日报和训练报告 system prompt 均包含“快速直接完成”和“不做冗长推理”。

- [ ] **步骤 2：运行测试确认失败**

运行：`go test ./app/agent/internal/agent ./app/agent/internal/biz/service -run 'Test.*Prompt|TestChat'`
预期：提示词测试 FAIL。

- [ ] **步骤 3：统一 agent 超时**

移除私有 90 秒常量，agent HTTP 客户端和单次调用 context 使用 `openaiclient.LLMCallTimeout`。AI 日报外层 context 和锁改为该常量；周报外层 8 分钟改为该常量，锁保持 10 分钟；训练报告任务使用该常量。

- [ ] **步骤 4：加入报告速度提示**

日报和训练报告 system prompt 增加“快速直接完成，不做冗长推理”，不修改 JSON 输出结构。

- [ ] **步骤 5：运行 agent 测试**

运行：`go test ./app/agent/...`
预期：PASS。

### 任务 4：回归验证与改动记录

**文件：**
- 修改：`../edits.md`

- [ ] **步骤 1：格式化修改文件**

运行：`gofmt -w app/common/openaiclient/client.go app/common/openaiclient/client_test.go app/core_data/internal/biz/service/problem_tagger.go app/core_data/internal/biz/service/problem_consumer.go app/core_data/internal/biz/service/problem_edit_test.go app/agent/internal/agent/chat.go app/agent/internal/biz/service/summary.go app/agent/internal/biz/service/summary_prompt.go app/agent/internal/biz/service/training_report.go app/agent/internal/biz/service/training_report_template.go app/agent/internal/biz/service/training_report_test.go`

- [ ] **步骤 2：运行目标回归**

运行：`go test ./app/common/openaiclient ./app/core_data/internal/biz/service ./app/agent/...`
预期：全部 PASS。

- [ ] **步骤 3：检查超时残留**

搜索 LLM 调用和 AI 任务 context，确认不存在 90 秒、3 分钟或 8 分钟的模型链路截断；非 AI 数据请求短超时保持不变。

- [ ] **步骤 4：记录改动**

向根目录 `edits.md` 追加公共 10 分钟 AI 超时、题库速度优先近似分类、报告速度提示及测试说明。
