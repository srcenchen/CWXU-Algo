# AI 原始推理流实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将兼容模型返回的原始推理流与最终 JSON 分开持久化和展示，并正确显示任务时间与完整 Prompt。

**架构：** 公共 OpenAI 流读取器从 SDK 保留的原始 delta JSON 提取 `reasoning_content`、`reasoning` 或 `reasoning_text`，通过独立回调传给题库任务记录。进度 API 新增 `reasoningOutput`，前端按思考、最终输出、完整 Prompt 分区展示。

**技术栈：** Go、openai-go v3、Protocol Buffers、Redis、React 19、TypeScript、shadcn/ui

---

### 任务 1：解析兼容推理流

**文件：**
- 修改：`app/common/openaiclient/client.go`
- 修改：`app/common/openaiclient/client_test.go`

- [ ] 先写表格测试，以原始 chunk JSON 覆盖 `reasoning_content`、字符串 `reasoning`、对象/数组 `reasoning`、`reasoning_text` 和未知字段。
- [ ] 运行 `go test ./app/common/openaiclient`，确认因提取函数缺失而失败。
- [ ] 实现 `reasoningDelta` 和带 reasoning/content 双回调的流读取函数；现有调用保持兼容。
- [ ] 重跑测试，确认通过。

### 任务 2：持久化推理与完整 Prompt

**文件：**
- 修改：`app/core_data/internal/biz/service/problem_pipeline.go`
- 修改：`app/core_data/internal/biz/service/problem_pipeline_test.go`
- 修改：`app/core_data/internal/biz/service/problem_tagger.go`
- 修改：`app/core_data/internal/biz/service/problem.go`

- [ ] 扩展失败测试，要求 Redis 记录分别保存 `reasoning_output`、`latest_output` 和完整 system/user prompt。
- [ ] 运行题库目标测试并确认失败。
- [ ] 为 `ActiveJob` 增加 `ReasoningOutput`、`TrackReasoning`，题库分析使用双回调累积；Prompt 记录 system 与 user 两部分。
- [ ] 重跑题库测试，确认通过。

### 任务 3：同步 API 契约

**文件：**
- 修改：`api/core/v1/problem/problem.proto`
- 生成：`api/core/v1/problem/problem.pb.go`
- 生成：`openapi.yaml`
- 修改：`app/core_data/internal/service/problem.go`
- 修改：`shared/api.ts`
- 修改：`shared/api.md`

- [ ] 在 `ActiveJob` 增加 `reasoning_output`，映射业务字段并运行 `make api`。
- [ ] 同步共享类型和文档的一小时保留语义。
- [ ] 运行相关 Go 编译测试。

### 任务 4：前端分区与时间修复

**文件：**
- 修改：`newUI-20260715/src/lib/format.ts`
- 创建：`newUI-20260715/src/lib/format.test.ts`
- 修改：`newUI-20260715/src/pages/dashboard/ProblemProgress.tsx`

- [ ] 先写 Node 测试，断言 RFC3339、Unix 秒和 Unix 毫秒都不会格式化为公元 1 年。
- [ ] 运行目标测试确认 RFC3339 用例失败。
- [ ] 修正 `formatTime`，并将任务抽屉拆为“思考过程”“最终输出”“完整 Prompt”；复制操作分别作用于对应内容。
- [ ] 运行前端测试和 `npm run build`。

### 任务 5：验证与记录

**文件：**
- 修改：`../edits.md`

- [ ] 运行 `gofmt`、后端目标测试和前端构建。
- [ ] 搜索确认 API、业务和 UI 都使用 `reasoningOutput`。
- [ ] 请求真实进度 API；若无 JWT，则验证 401 鉴权边界并明确记录限制。
- [ ] 向 `edits.md` 追加本轮全部改动，不提交不推送。
