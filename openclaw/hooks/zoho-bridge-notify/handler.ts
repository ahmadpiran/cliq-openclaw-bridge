// Declare Node globals for TypeScript — OpenClaw runs this in Node.js at runtime.
declare const process: { env: Record<string, string | undefined> };

const handler = async (event: Record<string, unknown>): Promise<void> => {
  const sessionKey = String(event["sessionKey"] ?? "");

  if (!sessionKey.includes("zoho-cliq")) return;

  const ctx = (event["context"] ?? {}) as Record<string, unknown>;
  const type = String(event["type"] ?? "");
  const action = String(event["action"] ?? "");

  console.log(
    `[zoho-bridge-notify] type=${type} action=${action} session=${sessionKey} ` +
    `ctxKeys=${Object.keys(ctx).join(",")}`
  );

  // --- message:sent path (fires when OpenClaw implements it) ---
  if (type === "message" && action === "sent") {
    const content = String(ctx["content"] ?? "").trim();
    if (!content || content === "NO_REPLY") return;
    if (ctx["success"] === false) return;
    await notifyBridge(content);
    return;
  }

  // --- before_message_write path (current fallback) ---
  if (type === "before_message_write") {
    const message = ctx["message"] as Record<string, unknown> | undefined;
    if (!message || message["role"] !== "assistant") return;

    const content = message["content"];
    let text = "";
    if (typeof content === "string") {
      text = content.trim();
    } else if (Array.isArray(content)) {
      for (const block of content) {
        const b = block as Record<string, unknown>;
        if (b["type"] === "text" && typeof b["text"] === "string") {
          text = (b["text"] as string).trim();
          break;
        }
      }
    }

    if (!text || text === "NO_REPLY") return;
    await notifyBridge(text);
    return;
  }
};

async function notifyBridge(text: string): Promise<void> {
  const bridgeUrl =
    process.env["BRIDGE_NOTIFY_URL"] ?? "http://cliq-bridge:8080/notify";
  const secret = process.env["BRIDGE_NOTIFY_SECRET"] ?? "";

  void fetch(bridgeUrl, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Notify-Secret": secret,
    },
    body: JSON.stringify({ text }),
  }).catch((err: unknown) => {
    console.error("[zoho-bridge-notify] failed to notify bridge:", err);
  });
}

export default handler;