# imap2pop

POP3 代理服务器，将飞书 IMAP 邮箱（imap.feishu.cn:993）转换为 POP3 协议，使传统邮件客户端可以通过 POP3 方式访问飞书邮箱。

## 功能

- 支持 POP3 标准命令：USER、PASS、STAT、LIST、RETR、DELE、UIDL、TOP、NOOP、RSET、QUIT、CAPA
- 通过 TLS 连接飞书 IMAP 服务器
- 每个客户端连接独立映射到一个 IMAP 会话
- 支持邮件删除（映射为 IMAP \Deleted 标记，QUIT 时 EXPUNGE）

## 使用

```bash
# 默认监听端口 1100
go run .

# 指定端口
go run . -port 110

# 环境变量方式
POP3_PORT=110 go run .
```

编译为二进制：

```bash
go build -o imap2pop .
./imap2pop -port 110
```

## ECS 部署

1. 编译：`go build -o imap2pop .`
2. 上传到 ECS
3. 创建 systemd 服务（参考 `imap2pop.service`）
4. 在安全组中开放 POP3 端口入方向 TCP

## 配置

| 参数 | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `-port` | `POP3_PORT` | 1100 | POP3 监听端口 |

IMAP 目标固定为 `imap.feishu.cn:993`。

## 日志

所有 POP3 和 IMAP 交互均通过日志输出，格式包含 `[远程地址]` session 标识：

```
2026/05/22 13:31:02.123456 [192.168.1.1:54321] POP3 <<< USER tian.ouyang@feishu.cn
2026/05/22 13:31:02.234567 [192.168.1.1:54321] POP3 >>> +OK User accepted
2026/05/22 13:31:02.345678 [192.168.1.1:54321] IMAP >>> A1 LOGIN "tian.ouyang@feishu.cn" "password"
2026/05/22 13:31:02.456789 [192.168.1.1:54321] IMAP <<< A1 OK LOGIN completed
```