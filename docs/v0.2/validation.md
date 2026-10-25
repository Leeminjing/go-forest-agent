# Responses 扩展验收

日期：2026-10-25。原 v0.1.0/提交 `20698e3` 保持不变；本次在独立可选树中实现 Responses 模型与原生续接 Journal，没有修改 agent/ 的接口或状态机。

Windows 全量测试、go vet，以及 Linux Go 1.23 的三轮 race 测试均通过。主要验证：

- 重开 Journal 后，完整原生 reasoning、phase、function_call 及其关联继续参与下一请求。
- 相同标准请求使用已保存结果，不重复发网络请求。
- 拒绝、不完整响应、断流、未知输出、只有 commentary 和 Context 取消不会成为成功执行计划。
- 真正终止生成进程：Journal 已保存、Agent 尚未提交结果时，另一进程复用原结果，API 请求计数保持一次。
- Focus-Go 集成验证原生续接经过领域封存时仍完整可读，业务参数按语义校验，浮点原生元数据不被整数领域 codec 改写。

真实网络验证使用用户授权的 DeepSeek Responses 兼容端点及 deepseek-v4-flash，完成代码搜索、读取、编辑、复查和 Context 结算；独立 Go 测试通过。不得将此称为 OpenAI 托管服务验收。API key、原始凭据文件和供应商原生输出均未上传。

标准协议依据：[OpenAI Function calling](https://developers.openai.com/api/docs/guides/function-calling)、[OpenAI Streaming](https://developers.openai.com/api/docs/guides/streaming-responses)；兼容端点差异依据：[DeepSeek Responses](https://api-docs.deepseek.com/guides/responses_api/)。默认 endpoint 保持 OpenAI，其他 endpoint 由可信宿主显式配置。

临时输出和完整模型结果严格区分。不承诺 API exactly-once、远端请求取消后零费用或本地断电耐久性；缺失/损坏续接记录明确失败。Spec/Design 与 scratch 不进入 Git 历史。
