package teams

// User-facing copy the bot speaks in Teams. Kept in one file so the wording
// (Thai first, English second — the team this fork serves) can be changed
// without touching logic. The agent's own replies are untouched by this file;
// they follow whatever language the user writes in.
const (
	msgWelcome = "👋 สวัสดีครับ ผมคือ agent ใน Multica — พิมพ์คุยกับผมได้เลย\n\n" +
		"- `/issue <ชื่องาน>` สร้าง issue ให้ agent นี้\n" +
		"- `/new` เริ่ม chat ใหม่ · `/clear` เริ่มรอบถัดไปแบบไม่มี context เดิม\n\n" +
		"ในช่องทีม (channel) ให้ @mention ผมก่อนทุกครั้ง"

	msgUnsupportedType = "ผมเปิดไฟล์นี้ไม่ได้ครับ — ไฟล์ที่แนบในช่องทีมหรือ group chat ต้องใช้สิทธิ์ SharePoint ที่ bot ไม่ได้ขอไว้ ส่งไฟล์มาในแชตส่วนตัวกับผมแทน (รูปที่ paste ลงแชตใช้ได้ทุกที่)"

	msgMediaUnavailable = "⚠️ บางไฟล์โหลดเข้า Multica ไม่สำเร็จ (ใหญ่เกิน 25 MB หรือดาวน์โหลดไม่ได้) agent จะเห็นแค่ชื่อไฟล์ ลองส่งใหม่อีกครั้งครับ"

	msgOutboundFilesHeader = "📎 ไฟล์แนบจาก agent:"
	msgOutboundFilesLink   = "[เปิดในแชตบน Multica](%s)"
	msgFilesDisabled       = "ระบบ Multica นี้ยังไม่ได้เปิดที่เก็บไฟล์ เลยรับรูปหรือไฟล์จาก Teams ไม่ได้ครับ ส่งเนื้อหาเป็นข้อความแทนได้"

	msgBindingPrompt = "👋 ก่อนเริ่มคุย ต้องผูกบัญชี Teams ของคุณกับ Multica ก่อนครับ:\n\n[ผูกบัญชี (ลิงก์หมดอายุใน 15 นาที)](%s)"

	msgBindingGroupHint = "ผมยังไม่รู้ว่าบัญชี Teams นี้คือใครใน Multica ครับ — เปิดแชตส่วนตัวกับผม (personal chat) แล้วพิมพ์อะไรก็ได้ เพื่อรับลิงก์ผูกบัญชี"

	msgNotMember = "บัญชีนี้ยังไม่ได้เป็นสมาชิก workspace นี้ใน Multica ครับ ให้ admin เชิญก่อนแล้วลองใหม่"

	msgAgentOffline  = "⚠️ agent นี้ออฟไลน์อยู่ (ไม่มี runtime ที่เชื่อมต่อ) ข้อความถูกบันทึกไว้แล้ว จะทำงานเมื่อ runtime กลับมาออนไลน์"
	msgAgentArchived = "⚠️ agent นี้ถูก archive แล้ว ให้ admin เชื่อม bot นี้กับ agent อื่นใน Multica"

	msgFreshPending  = "✅ เริ่มใหม่แล้ว ข้อความถัดไปจะไม่ใช้ context เดิม"
	msgChatStarted   = "✅ เริ่ม chat ใหม่ใน Multica แล้ว ข้อความถัดไปจะเข้า chat นี้"
	msgIssueUsage    = "ใส่ชื่อ issue ด้วยครับ เช่น\n\n`/issue <ชื่องาน>`\n[รายละเอียด] (ไม่ใส่ก็ได้)"
	msgIssueDisabled = "bot นี้ไม่ได้เชื่อมกับ Multica แล้ว ให้ admin เชื่อมใหม่"

	msgTaskFailed = "❌ agent ทำงานรอบนี้ไม่สำเร็จ ลองส่งใหม่อีกครั้ง หรือดูรายละเอียดใน Multica"

	msgIssueCreatedFmt   = "✅ สร้าง %s แล้ว"
	msgIssueCreatedTitle = "✅ สร้าง %s แล้ว — %s"
	msgIssueDupFmt       = "⚠️ ไม่ได้สร้างใหม่ — มี issue %s ที่ยังเปิดอยู่แล้ว"
	msgIssueDupTitle     = "⚠️ ไม่ได้สร้างใหม่ — มี issue %s ที่ยังเปิดอยู่แล้ว: %s"
)
