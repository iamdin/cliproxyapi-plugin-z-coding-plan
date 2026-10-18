# z-coding-plan

CLIProxyAPI plugin for Zhipu GLM Coding Plan. It shows each key's quota, and skips a key after Zhipu returns error 1308 until the reset time in that response.

While every key is healthy, the plugin does not choose the key. The host scheduler stays in control.

## Install

Build the shared library:

    CGO_ENABLED=1 go build -buildmode=c-shared -o z-coding-plan.so .

Put it in the plugin directory. On Linux amd64 that is plugins/linux/amd64/z-coding-plan.so.

    plugins:
      enabled: true
      configs:
        z-coding-plan:
          enabled: true
          priority: 1

Restart CLIProxyAPI after replacing the library. The host does not unload an old copy inside the same process.

The optional endpoint field overrides the quota URL. The default is https://bigmodel.cn/api/monitor/usage/quota/limit.

## Quota credentials

Add one auth file per key in the host auth directory. These files are for the quota page. Routing keys stay in the host's normal upstream configuration.

    {
      "type": "z-coding-plan",
      "label": "Key 1",
      "api_key": "YOUR_ZHIPU_KEY"
    }

The quota page lists the file under provider z-coding-plan. Legacy plans are shown as token windows. The current credit plan is shown as credits. MCP is shown separately. A five-hour reset that is more than five hours and one minute ahead is hidden instead of guessing a timezone.

## 1308 failover

When a routed request fails with Zhipu error 1308 and the message includes a reset time, the plugin remembers that credential id until then. Digits buried in a longer number, or a 1308 message with no reset time, do not block a key. The next selection skips it. If every candidate is blocked, the one that resets soonest is used. Other errors do not block a key. The block list is in memory and clears when the process exits.

The id comes from the host. Failover follows the credential that actually served the request, which is not automatically the quota auth file.

## Plugin store

Publish a GitHub release tagged v0.1.0. Include checksums.txt and one zip per platform. The zip root contains only the library, for example z-coding-plan.so inside z-coding-plan_0.1.0_linux_amd64.zip.

Registry fields: id z-coding-plan, name Z Coding Plan, author oxwen11, repository https://github.com/oxwen11/cliproxyapi-plugin-z-coding-plan, license MIT.
