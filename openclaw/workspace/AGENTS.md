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
├── memory/        ← session memory (managed by session-memory hook)
└── ...            ← your working directory for generated files

### File size limits

- OpenClaw pdf tool: up to 100 MB (configured via pdfMaxBytesMb)
- Zoho Cliq file upload: up to 100 MB per file

### Session continuity

Each Zoho Cliq conversation uses session key `hook:zoho-cliq`. Context
is preserved within a session. Use `/new` to start a fresh session.