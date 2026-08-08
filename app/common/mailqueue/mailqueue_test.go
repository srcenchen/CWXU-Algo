package mailqueue

import "testing"

func TestEnqueueRejectsInvalidInput(t *testing.T) {
	// nil mq / 空收件人 / 空主题一律不入队
	if Enqueue(nil, "a@b.c", "subject", "<p>x</p>") {
		t.Fatal("nil mq should not enqueue")
	}
	if Enqueue(nil, "", "subject", "<p>x</p>") {
		t.Fatal("empty to should not enqueue")
	}
	if Enqueue(nil, "a@b.c", "  ", "<p>x</p>") {
		t.Fatal("blank subject should not enqueue")
	}
}
