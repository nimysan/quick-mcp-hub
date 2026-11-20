// Package applemail 只读地读取 macOS「邮件」(Apple Mail) 的本地存储，
// 不连 IMAP/EWS 服务器，不需要账户授权，也绝不写任何文件。
//
// 磁盘布局（macOS 13–26 上为 V10，旧版本为 V2…V9，代码取最大的那个）：
//
//	~/Library/Mail/V<n>/
//	  MailData/Envelope Index          SQLite，元数据索引（Mail.app 以 WAL 模式持有）
//	  <账户UUID>/                      IMAP/Exchange/iCloud 账户，一个账户一个目录
//	    INBOX.mbox/                    邮箱；子邮箱嵌套为 父.mbox/子.mbox
//	      <存储UUID>/Data/<d>/<d>/<d>/Messages/<ROWID>.emlx
//	      <存储UUID>/Data/<d>/.../Attachments/<ROWID>/<部件号>/<文件名>
//	  Mailboxes/                       「我的 Mac 上」本地邮箱
//
// Data 下的分桶目录：把 ROWID 去掉末三位后逐位倒序，例如 ROWID 194184 →
// Data/4/9/1/Messages/194184.emlx；ROWID < 1000 时直接是 Data/Messages/。
//
// Envelope Index 里关心的表（列按需探测，不假设一定存在）：
//
//	messages(ROWID, subject→subjects, sender→addresses, mailbox→mailboxes,
//	         date_sent, date_received, read, flagged, deleted, size, summary→summaries)
//	subjects(ROWID, subject)   addresses(ROWID, address, comment=显示名)
//	mailboxes(ROWID, url, total_count, unread_count)   url 形如 imap://<账户UUID>/INBOX
//	recipients(message, address, type 0=To 1=Cc 2=Bcc, position)
//	attachments(message, attachment_id, name)
//
// .emlx：第一行是十进制字节数 N，随后 N 字节是 RFC 5322/MIME 原文，再往后是
// XML plist（flags 等）。.partial.emlx 是附件被剥离的版本，附件本体单独放在
// 同级 Attachments/<ROWID>/ 下。
package applemail
