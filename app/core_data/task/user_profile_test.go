package task

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"cwxu-algo/app/common/event"

	"github.com/streadway/amqp"
)

type recordingProfilePublisher struct {
	mu       sync.Mutex
	events   []event.UserProfileEvent
	bodies   [][]byte
	failNext bool
}

type racingForcePublisher struct {
	mu                sync.Mutex
	events            []event.UserProfileEvent
	firstForceStarted chan struct{}
	releaseFirstForce chan struct{}
	forceCalls        int
}

type racingNormalPublisher struct {
	mu                 sync.Mutex
	events             []event.UserProfileEvent
	firstNormalStarted chan struct{}
	releaseFirstNormal chan struct{}
	normalCalls        int
}

func (p *racingNormalPublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *racingNormalPublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	var profileEvent event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &profileEvent); err != nil {
		return err
	}
	p.mu.Lock()
	p.normalCalls++
	call := p.normalCalls
	p.mu.Unlock()
	if call == 1 {
		close(p.firstNormalStarted)
		<-p.releaseFirstNormal
		return errors.New("first normal publish failed")
	}
	p.mu.Lock()
	p.events = append(p.events, profileEvent)
	p.mu.Unlock()
	return nil
}

func (p *racingForcePublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *racingForcePublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	var profileEvent event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &profileEvent); err != nil {
		return err
	}
	if !profileEvent.Force {
		p.mu.Lock()
		p.events = append(p.events, profileEvent)
		p.mu.Unlock()
		return nil
	}
	p.mu.Lock()
	p.forceCalls++
	call := p.forceCalls
	p.mu.Unlock()
	if call == 1 {
		close(p.firstForceStarted)
		<-p.releaseFirstForce
		return errors.New("first force publish failed")
	}
	p.mu.Lock()
	p.events = append(p.events, profileEvent)
	p.mu.Unlock()
	return nil
}

func (p *recordingProfilePublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *recordingProfilePublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNext {
		p.failNext = false
		return errors.New("forced publish failure")
	}
	var profileEvent event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &profileEvent); err != nil {
		return err
	}
	p.events = append(p.events, profileEvent)
	p.bodies = append(p.bodies, append([]byte(nil), msg.Body...))
	return nil
}

func (p *recordingProfilePublisher) snapshot() []event.UserProfileEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]event.UserProfileEvent(nil), p.events...)
}

func (p *recordingProfilePublisher) snapshotBodies() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.bodies))
	for i := range p.bodies {
		out[i] = append([]byte(nil), p.bodies[i]...)
	}
	return out
}

func TestUserProfileTaskPublishedClaimCarriesOwnershipToken(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
		state string
	}{
		{name: "normal", state: userProfilePendingNormal},
		{name: "force", force: true, state: userProfilePendingForce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rdb := newTestRedis(t)
			pub := &recordingProfilePublisher{}
			profileTask := NewUserProfileTaskWithPublisher(pub, rdb)
			const userID = int64(48)
			var result UserProfileEnqueueResult
			if tc.force {
				result = profileTask.DoForce(userID)
			} else {
				result = profileTask.Do(userID)
			}
			if !result.Published || result.Deduped || result.Failed {
				t.Fatalf("enqueue=%+v", result)
			}
			bodies := pub.snapshotBodies()
			if len(bodies) != 1 {
				t.Fatalf("published bodies=%d", len(bodies))
			}
			var envelope struct {
				ClaimToken string `json:"claim_token"`
			}
			if err := json.Unmarshal(bodies[0], &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.ClaimToken == "" {
				t.Fatal("ordinary profile event omitted its pending claim token")
			}
			want := tc.state + ":published:" + envelope.ClaimToken
			if got := rdb.Get(t.Context(), userProfilePendingKey(userID)).Val(); got != want {
				t.Fatalf("published pending=%q want=%q", got, want)
			}
		})
	}
}

func TestUserProfileTaskForceDedupesTokenizedPublishedClaim(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &recordingProfilePublisher{}
	profileTask := NewUserProfileTaskWithPublisher(pub, rdb)
	const userID = int64(49)
	if err := rdb.Set(t.Context(), userProfilePendingKey(userID), "force:published:existing-token", userProfilePendingTTL).Err(); err != nil {
		t.Fatal(err)
	}
	if result := profileTask.DoForce(userID); !result.Deduped || result.Published || result.Failed {
		t.Fatalf("tokenized force pending was not deduped: %+v", result)
	}
	if events := pub.snapshot(); len(events) != 0 {
		t.Fatalf("deduped force published events=%+v", events)
	}
}

func TestUserProfileTaskForceUpgradesNormalPending(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &recordingProfilePublisher{}
	task := NewUserProfileTaskWithPublisher(pub, rdb)

	if got := task.Do(41); !got.Published || got.Failed {
		t.Fatalf("normal enqueue=%+v", got)
	}
	if got := task.DoForce(41); !got.Published || got.Deduped || got.Failed {
		t.Fatalf("force must publish an upgrade, got %+v", got)
	}
	events := pub.snapshot()
	if len(events) != 2 || events[0].Force || !events[1].Force || events[1].ClaimToken == "" {
		t.Fatalf("published events=%+v", events)
	}
	wantPending := "force:published:" + events[1].ClaimToken
	if got := rdb.Get(t.Context(), userProfilePendingKey(41)).Val(); got != wantPending {
		t.Fatalf("pending state=%q want=%q", got, wantPending)
	}
}

func TestUserProfileTaskMaintenanceForceCarriesIntentAndBypassesPendingDedup(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &recordingProfilePublisher{}
	task := NewUserProfileTaskWithPublisher(pub, rdb)
	if got := task.DoForce(47); !got.Published {
		t.Fatalf("ordinary force enqueue=%+v", got)
	}
	if got := task.DoMaintenanceForce(47, "maintenance-intent"); !got.Published || got.Deduped || got.Failed {
		t.Fatalf("maintenance enqueue=%+v", got)
	}
	events := pub.snapshot()
	if len(events) != 2 || events[0].ClaimToken == "" || events[1].ClaimToken != "" || events[1].IntentID != "maintenance-intent" || !events[1].Force {
		t.Fatalf("maintenance event=%+v", events)
	}
	if got := rdb.Get(t.Context(), userProfilePendingKey(47)).Val(); got != "force:published:"+events[0].ClaimToken {
		t.Fatalf("maintenance enqueue must not alter ordinary pending state=%q", got)
	}
}

func TestUserProfileTaskPublishFailureAndCompletionAllowReenqueue(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &recordingProfilePublisher{failNext: true}
	task := NewUserProfileTaskWithPublisher(pub, rdb)

	if got := task.Do(42); !got.Failed || got.KeepClaim() {
		t.Fatalf("failed enqueue=%+v", got)
	}
	if rdb.Exists(t.Context(), userProfilePendingKey(42)).Val() != 0 {
		t.Fatal("publish failure retained pending claim")
	}
	if got := task.Do(42); !got.Published {
		t.Fatalf("retry after failure=%+v", got)
	}
	events := pub.snapshot()
	if len(events) != 1 {
		t.Fatalf("published events=%+v", events)
	}
	if err := task.ClearPending(42, events[0].Force, events[0].ClaimToken); err != nil {
		t.Fatal(err)
	}
	if got := task.Do(42); !got.Published {
		t.Fatalf("reenqueue after completion=%+v", got)
	}
}

func TestUserProfileTaskFailedForceUpgradeReleasesClaim(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &recordingProfilePublisher{}
	task := NewUserProfileTaskWithPublisher(pub, rdb)
	if got := task.Do(43); !got.Published {
		t.Fatalf("normal enqueue=%+v", got)
	}
	pub.mu.Lock()
	pub.failNext = true
	pub.mu.Unlock()
	if got := task.DoForce(43); !got.Failed || got.KeepClaim() {
		t.Fatalf("failed force upgrade=%+v", got)
	}
	if rdb.Exists(t.Context(), userProfilePendingKey(43)).Val() != 0 {
		t.Fatal("failed force upgrade retained pending claim")
	}
	if got := task.DoForce(43); !got.Published {
		t.Fatalf("force retry after released upgrade=%+v", got)
	}
	events := pub.snapshot()
	if len(events) != 2 || events[0].Force || !events[1].Force {
		t.Fatalf("published events=%+v", events)
	}
}

func TestUserProfileTaskConcurrentForceDoesNotDedupTentativePublish(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &racingForcePublisher{
		firstForceStarted: make(chan struct{}),
		releaseFirstForce: make(chan struct{}),
	}
	task := NewUserProfileTaskWithPublisher(pub, rdb)
	if got := task.Do(44); !got.Published {
		t.Fatalf("normal enqueue=%+v", got)
	}
	firstResult := make(chan UserProfileEnqueueResult, 1)
	go func() { firstResult <- task.DoForce(44) }()
	<-pub.firstForceStarted

	second := task.DoForce(44)
	close(pub.releaseFirstForce)
	first := <-firstResult
	if !first.Failed {
		t.Fatalf("first force result=%+v", first)
	}
	if !second.Published || second.Deduped || second.Failed {
		t.Fatalf("second force treated tentative publish as safe dedup: %+v", second)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	forceEvents := 0
	for _, e := range events {
		if e.Force {
			forceEvents++
		}
	}
	if forceEvents == 0 {
		t.Fatalf("no force event survived concurrent publish failure: %+v", events)
	}
}

func TestUserProfileTaskConcurrentNormalDoesNotDedupTentativePublish(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &racingNormalPublisher{
		firstNormalStarted: make(chan struct{}),
		releaseFirstNormal: make(chan struct{}),
	}
	task := NewUserProfileTaskWithPublisher(pub, rdb)
	firstResult := make(chan UserProfileEnqueueResult, 1)
	go func() { firstResult <- task.Do(45) }()
	<-pub.firstNormalStarted

	second := task.Do(45)
	close(pub.releaseFirstNormal)
	first := <-firstResult
	if !first.Failed {
		t.Fatalf("first normal result=%+v", first)
	}
	if !second.Published || second.Deduped || second.Failed {
		t.Fatalf("second normal treated tentative publish as safe dedup: %+v", second)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 1 || events[0].Force {
		t.Fatalf("no normal event survived concurrent publish failure: %+v", events)
	}
}

func TestUserProfileTaskConcurrentNormalTakesOverFailingTentativeForce(t *testing.T) {
	_, rdb := newTestRedis(t)
	pub := &racingForcePublisher{
		firstForceStarted: make(chan struct{}),
		releaseFirstForce: make(chan struct{}),
	}
	task := NewUserProfileTaskWithPublisher(pub, rdb)
	firstResult := make(chan UserProfileEnqueueResult, 1)
	go func() { firstResult <- task.DoForce(46) }()
	<-pub.firstForceStarted

	second := task.Do(46)
	close(pub.releaseFirstForce)
	first := <-firstResult
	if !first.Failed {
		t.Fatalf("first force result=%+v", first)
	}
	if !second.Published || second.Deduped || second.Failed {
		t.Fatalf("normal treated tentative force as safe dedup: %+v", second)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 1 || !events[0].Force {
		t.Fatalf("normal takeover did not preserve force semantics: %+v", events)
	}
}
