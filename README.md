# z-coding-plan

CLIProxyAPI 插件。展示智谱 GLM Coding Plan 额度，并在智谱返回 1308 且带重置时间时，停用那把实际发出请求的凭证，直到该时间。

只支持中国区 BigModel。额度固定请求 https://bigmodel.cn/api/monitor/usage/quota/limit。不支持全球区 api.z.ai，也没有额度地址配置项。中国区 Key 不能拿到全球区使用，反过来也不行。

这里有两份不同的凭证，不要混用：

- 额度认证文件是插件解析的 JSON。它只让管理页的额度页能显示剩余额度，本身不是路由 Key。
- 路由凭证是 CLIProxyAPI 用来请求智谱的上游 API Key。1308 切换跟着实际发出那次请求的凭证 id 走。

## 配置

在 CLIProxyAPI 的配置目录操作，下面称为 CONFIG。认证目录是宿主的 auth-dir，常见是 ~/.cli-proxy-api。

1. 编译动态库，放到宿主加载插件的目录。文件名就是插件 id。

        CGO_ENABLED=1 go build -buildmode=c-shared -o z-coding-plan.so .

   Linux amd64 的目标路径是 CONFIG/plugins/linux/amd64/z-coding-plan.so。

   完成标准：该路径存在，且文件名正好是 z-coding-plan.so。

2. 在 CONFIG/config.yaml 里启用插件。宿主读 enabled 和 priority；min-concurrency、max-concurrency 见下文 1302 限流，quota-warmup-times、quota-warmup-model 见下文额度预热，都可省略。

        plugins:
          enabled: true
          configs:
            z-coding-plan:
              enabled: true
              priority: 1
              min-concurrency: 8
              max-concurrency: 64
              quota-warmup-times: "03:00,08:00,13:00,18:00,23:00"

   完成标准：plugins.enabled 为 true，且 plugins.configs.z-coding-plan.enabled 为 true。

3. 每个智谱 Coding Plan Key 在认证目录里放一个文件，权限 0600。一个文件一把 Key。

        {
          "type": "z-coding-plan",
          "label": "Key 1",
          "api_key": "智谱上游 Key"
        }

   type 必须是字符串 z-coding-plan。api_key 必填，填智谱上游 Key，不要填 CLIProxyAPI 的客户端访问密钥。api-key 也可以当作别名。label 可省略，省略时用去掉 .json 的文件名。宿主看到的凭证 id 是去掉 .json 的文件名。

   完成标准：每个文件都是合法 JSON，api_key 非空，type 正好是 z-coding-plan。

4. 请求路由仍放在宿主已有的上游 Key 列表里。本插件不复制这些 Key，也不会自己发模型请求。额度认证文件不会变成路由 Key。

   完成标准：要承接 glm-5.3 流量的同一批智谱 Key，仍然留在宿主的上游配置里。

5. 重启 CLIProxyAPI。替换 .so 不会影响已经加载它的进程。

   完成标准：日志里出现 plugin registered plugin_id=z-coding-plan。

6. 用管理密钥检查管理接口。

   - GET /v0/management/plugins 里有 z-coding-plan，registered 为 true，supports_quota 为 true。
   - GET /v0/management/auth-files 列出每个新文件，provider 为 z-coding-plan，supports_quota 为 true，并带 auth_index。
   - POST /v8/management/plugins/z-coding-plan/quota，请求体为 {"auth_index":"<auth_index>"}，返回套餐名和窗口。Token 窗口标为额度，积分窗口标为积分，MCP 标为 MCP。

   完成标准：每个额度认证文件都返回窗口，或者返回的错误不是 quota credential not found。

配置成功后，管理页的额度页会列出每把中国区 Coding Plan Key。卡片上的套餐名来自接口，5 小时和 MCP 是两个窗口。数字会随用量变化，下面只是一次成功配置的样子。

![额度页：两把中国区智谱 Coding Plan Key](docs/quota-page.png)

## 原样转发

插件同时是 glm-5.3、glm-5.3-flash 的执行器：路由到 z-coding-plan 凭证的 Anthropic Messages 请求，请求体按客户端发来的字节原样发到 https://open.bigmodel.cn/api/anthropic/v1/messages，客户端请求头原样带上，只把鉴权换成该凭证的 Key（X-Api-Key），并去掉逐跳头和转发头；响应按 SSE 事件原样流回。智谱的错误状态码和正文原样交回宿主，1302、1308 处理照旧。

CPA 自带的 claude 执行器会改写请求（Claude Code 伪装、删 thinking、工具改名、地址加 ?beta=true），要让经 CPA 的调用与直连一致，就让 glm 只走本插件：

- api-keys.claude 里承接智谱的条目用 excluded-models 排除这两个模型（或删掉条目），否则宿主会在两条路之间轮换；
- 1302 换 Key 不冷却：在 oauth.request-scoped-errors 下给 z-coding-plan 配 status 429、match "1302"、action continue。

messages-endpoint 可改转发地址，只用于核对时指向本地录制转发，平时不要配。

## 1308 切换

所有路由凭证都正常时，插件不参与选 Key。

请求失败后，只有错误文本里的 1308 是独立数字，并且带有 YYYY-MM-DD HH:MM:SS 时间戳，才会停用那把凭证。时间按中国时间（UTC+8）读取，停用到该时刻。130812 不算。只有 1308、没有时间戳，也不停用。其他错误不停用。

下一次选择会跳过已停用的凭证 id。如果候选全部停用，就用重置时间最早的那把。停用名单只在内存里，进程退出或插件卸载后清空。

被停用的 id 是实际发出请求的宿主凭证 id，不是额度文件名，除非宿主路由用的就是那个文件。

## 1302 限流

候选全是智谱 Key（base_url 含 bigmodel.cn）时，插件按 Key 控制并发。每把 Key 起始允许 min-concurrency 个并发请求（默认 8），每成功一次慢慢加，最多 max-concurrency 个（默认 64）。宿主注册或重新配置插件时读取这两个值。

收到 1302 时，这把 Key 的并发上限减半（不低于 min-concurrency），并暂停一段时间：2 秒起，连续限流翻倍，最长 60 秒，带随机抖动。同一波里同时回来的多个 1302 只算一次。暂停到期后成功一次，退避重新从 2 秒算。

没有空位的请求在选 Key 时排队，每 200 毫秒看一次，最多等 90 秒，超时后交回宿主。所有候选都因 1308 停用时不排队。占用的并发位置在请求结束时释放，5 分钟内没有结束记录也会自动释放。这些状态同样只在内存里。

## 额度预热（quota-warmup）

智谱 5 小时额度从窗口内第一次调用开始计时。配置 quota-warmup-times 后，插件每天在这些北京时间点，用每个 z-coding-plan 额度认证文件里的 Key 直接向智谱发一个 max_tokens 为 1 的请求（模型默认 glm-5.3，可用 quota-warmup-model 改），提前开启窗口。留空或省略即关闭。

到点时先查这把 Key 的 5 小时窗口：1 小时内会重置的，等重置后 3 秒再发；还要 1 小时以上才重置的，这次跳过。每次结果写进 CPA 日志，前缀 quota-warmup。

推荐 03:00,08:00,13:00,18:00,23:00：14:00 到 18:00 的双倍消耗时段整个落在 13:00 开始的窗口里，18:00 一到就是新窗口。

## 发布

发布 tag 为 v0.1.0。附上 checksums.txt，以及每个平台一个 zip。zip 根目录只放动态库，例如 z-coding-plan_0.1.0_linux_amd64.zip 里直接是 z-coding-plan.so。

登记字段：id 为 z-coding-plan，name 为 Z Coding Plan，author 为 iamdin，repository 为 https://github.com/iamdin/cliproxyapi-plugin-z-coding-plan，license 为 MIT。
