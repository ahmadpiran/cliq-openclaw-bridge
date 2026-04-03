## Zoho Cliq Integration

This agent receives messages from Zoho Cliq via the cliq-openclaw-bridge.
Replies are delivered back to the user's Zoho Cliq chat automatically.

### Sending files to Zoho Cliq

When you produce a file the user should receive, include this tag at the
end of your reply on its own line:

    [FILE:~/workspace/path/to/file.pdf]

The bridge will upload the file directly to the Zoho Cliq chat. You may
include text before the tag — it will be sent as a separate message first.

**Example reply:**

    I've analysed the document. Here is a summary of the key findings:

    - Finding 1: ...
    - Finding 2: ...

    I've also prepared a detailed report:

    [FILE:~/workspace/uploads/analysis_report.pdf]

**Supported file types:** PDF, images, text files, CSV, Word documents, and
most common formats. Files must exist in `~/workspace/` before you reference
them.

### Workspace layout
~/workspace/
├── uploads/       ← files sent by the user arrive here
├── memory/        ← persistent memory files (survive context window rollovers)
└── ...            ← your working directory for generated files

### Memory

When the user shares important facts, project specs, decisions, names, dates,
preferences, or anything they may ask about later — write a summary to
`~/workspace/memory/<topic>.md` immediately. Do not wait for `/new` to be
issued. The context window is finite; memory files are not.

Examples of what to persist proactively:
- Project specs or architecture decisions ("memory/skynet-light.md")
- Case details or client names ("memory/cases.md")
- User preferences or standing instructions ("memory/preferences.md")
- Any fact the user explicitly tells you to remember

### Session continuity

Each Zoho Cliq channel has its own isolated session key (`zoho-cliq:<channelID>`),
so conversations in different channels do not share context. Use `/new` to
start a fresh session within a channel.

### File size limits

- OpenClaw pdf tool: up to 100 MB (configured via pdfMaxBytesMb)
- Zoho Cliq file upload: up to 100 MB per file