# 排队题目任务展示实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在任务面板展示题面获取和 AI 分析的排队题目。

**架构：** 前端从现有 `inProgress` 派生排队行，并按题目 ID 与 `activeJobs` 去重。执行中行保留实时详情入口，排队行只显示阶段和“排队中”。

**技术栈：** React 19、TypeScript、shadcn/ui、Node test runner

---

### 任务 1：排队任务派生逻辑

**文件：**
- 创建：`newUI-20260715/src/lib/problem-queue-jobs.ts`
- 创建：`newUI-20260715/src/lib/problem-queue-jobs.test.ts`

- [ ] 先写失败测试，覆盖执行任务去重、`FETCHING`/`TAGGING` 映射和其他状态过滤。
- [ ] 运行 `npx tsx --test src/lib/problem-queue-jobs.test.ts` 确认失败。
- [ ] 实现纯函数并重跑测试。

### 任务 2：任务面板接入

**文件：**
- 修改：`newUI-20260715/src/pages/dashboard/ProblemProgress.tsx`
- 修改：`newUI-20260715/package.json`

- [ ] 合并执行中和排队中行，更新标题、数量和空状态。
- [ ] 排队行仅显示 `题面获取 · 排队中` 或 `AI 分析 · 排队中`，不打开实时详情。
- [ ] 将新测试加入 `npm test`，运行完整测试和生产构建。

### 任务 3：记录

**文件：**
- 修改：`edits.md`

- [ ] 追加排队任务展示与测试说明，保持未提交。
