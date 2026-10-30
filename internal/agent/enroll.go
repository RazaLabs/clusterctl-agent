package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/RazaLabs/clusterctl-agent/internal/pairing"
)

// enrolledKey is the state ConfigMap entry remembering a done enrollment: the cluster's relay
// id and a hash of the enrollment key, so a new relay key or ENROLL_KEY enrolls again.
const enrolledKey = "enrolled"

type enrollment struct {
	Cluster string `json:"cluster"`
	Key     string `json:"key"` // pairing.RelayID(ENROLL_KEY): never the key itself
	Org     string `json:"org"`
	OrgName string `json:"orgName"`
}

// errEnrollRefused is a final answer from members: retrying won't help.
type errEnrollRefused struct {
	status int
	body   string
}

func (e errEnrollRefused) Error() string {
	return fmt.Sprintf("members refused the enrollment (%d): %s", e.status, e.body)
}

// enroll joins the cluster to the organization of ENROLL_KEY at members.razalabs.com, once per
// relay key, so its alerts reach every signed-in member's phone. Network trouble is retried
// with backoff until ctx ends.
func (a *Agent) enroll(ctx context.Context) {
	if a.cfg.EnrollKey == "" {
		return
	}
	cluster := a.pairing.ClusterID()
	keyID := pairing.RelayID(a.cfg.EnrollKey)
	if e, ok := a.enrolled(ctx); ok && e.Cluster == cluster && e.Key == keyID {
		slog.Info("enrolled already", "org", e.OrgName)
		return
	}
	backoff := a.enrollBackoff
	if backoff == 0 {
		backoff = 5 * time.Second
	}
	for {
		e, err := a.postEnroll(ctx, cluster)
		if err == nil {
			e.Cluster, e.Key = cluster, keyID
			a.rememberEnrollment(ctx, e)
			slog.Info("enrolled in the organization; its signed-in members get this cluster's alerts", "org", e.OrgName)
			return
		}
		var r errEnrollRefused
		if errors.As(err, &r) {
			if r.status == http.StatusUnauthorized {
				slog.Error("ENROLL_KEY was refused: it's wrong or was revoked. Make a new one in your team's settings on members.razalabs.com, put it in the clusterctl-enroll Secret and restart the agent.", "status", r.status)
			} else {
				slog.Error("enrolling failed", "err", err)
			}
			return
		}
		slog.Warn("enrolling failed, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (a *Agent) postEnroll(ctx context.Context, cluster string) (enrollment, error) {
	body, _ := json.Marshal(map[string]string{"key": a.cfg.EnrollKey, "cluster": cluster, "name": a.pairing.Cluster})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.MembersURL+"/v1/clusterctl/enroll", bytes.NewReader(body))
	if err != nil {
		return enrollment{}, errEnrollRefused{status: 0, body: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return enrollment{}, err
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	switch {
	case res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests:
		return enrollment{}, fmt.Errorf("members answered %d: %s", res.StatusCode, text)
	case res.StatusCode >= 300:
		return enrollment{}, errEnrollRefused{status: res.StatusCode, body: string(text)}
	}
	var e enrollment
	if err := json.Unmarshal(text, &e); err != nil {
		return enrollment{}, fmt.Errorf("members' answer: %w", err)
	}
	return e, nil
}

func (a *Agent) enrolled(ctx context.Context) (enrollment, bool) {
	cm, err := a.kube.CoreV1().ConfigMaps(a.cfg.Namespace).Get(ctx, stateName, metav1.GetOptions{})
	if err != nil {
		return enrollment{}, false
	}
	var e enrollment
	if json.Unmarshal([]byte(cm.Data[enrolledKey]), &e) != nil {
		return enrollment{}, false
	}
	return e, true
}

func (a *Agent) rememberEnrollment(ctx context.Context, e enrollment) {
	raw, _ := json.Marshal(e)
	data := map[string]string{enrolledKey: string(raw)}
	cms := a.kube.CoreV1().ConfigMaps(a.cfg.Namespace)
	patch, _ := json.Marshal(map[string]any{"data": data})
	for range 2 {
		_, err := cms.Patch(ctx, stateName, types.MergePatchType, patch, metav1.PatchOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cms.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: a.cfg.Namespace},
				Data:       data,
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue // written meanwhile: patch it
			}
		}
		if err != nil {
			slog.Warn("remembering the enrollment", "err", err)
		}
		return
	}
}
