package channels

import (
	"context"
	"encoding/json"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"

	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
)

type ExtendedAlert struct {
	Status        string             `json:"status"`
	Labels        template.KV        `json:"labels"`
	Annotations   template.KV        `json:"annotations"`
	StartsAt      time.Time          `json:"startsAt"`
	EndsAt        time.Time          `json:"endsAt"`
	GeneratorURL  string             `json:"generatorURL"`
	Fingerprint   string             `json:"fingerprint"`
	SilenceURL    string             `json:"silenceURL"`
	DashboardURL  string             `json:"dashboardURL"`
	PanelURL      string             `json:"panelURL"`
	Values        map[string]float64 `json:"values"`
	ValueString   string             `json:"valueString"` // TODO: Remove in Grafana 10
	ImageURL      string             `json:"imageURL,omitempty"`
	EmbeddedImage string             `json:"embeddedImage,omitempty"`
}

type ExtendedAlerts []ExtendedAlert

type ExtendedData struct {
	Receiver string         `json:"receiver"`
	Status   string         `json:"status"`
	Alerts   ExtendedAlerts `json:"alerts"`

	GroupLabels       template.KV `json:"groupLabels"`
	CommonLabels      template.KV `json:"commonLabels"`
	CommonAnnotations template.KV `json:"commonAnnotations"`

	ExternalURL string `json:"externalURL"`
}

func removePrivateItems(kv template.KV) template.KV {
	for key := range kv {
		if strings.HasPrefix(key, "__") && strings.HasSuffix(key, "__") {
			kv = kv.Remove([]string{key})
		}
	}
	return kv
}

func extendAlert(alert template.Alert, externalURL string, logger Logger) *ExtendedAlert {
	// remove "private" annotations & labels so they don't show up in the template
	extended := &ExtendedAlert{
		Status:       alert.Status,
		Labels:       removePrivateItems(alert.Labels),
		Annotations:  removePrivateItems(alert.Annotations),
		StartsAt:     alert.StartsAt,
		EndsAt:       alert.EndsAt,
		GeneratorURL: alert.GeneratorURL,
		Fingerprint:  alert.Fingerprint,
	}

	// fill in some grafana-specific urls
	if len(externalURL) == 0 {
		return extended
	}
	u, err := url.Parse(externalURL)
	if err != nil {
		logger.Debug("failed to parse external URL while extending template data", "url", externalURL, "error", err.Error())
		return extended
	}
	externalPath := u.Path
	dashboardUid := alert.Annotations[ngmodels.DashboardUIDAnnotation]
	if len(dashboardUid) > 0 {
		u.Path = path.Join(externalPath, "/d/", dashboardUid)
		extended.DashboardURL = u.String()
		panelId := alert.Annotations[ngmodels.PanelIDAnnotation]
		if len(panelId) > 0 {
			u.RawQuery = "viewPanel=" + panelId
			extended.PanelURL = u.String()
		}

		generatorUrl, err := url.Parse(extended.GeneratorURL)
		if err != nil {
			logger.Debug("failed to parse generator URL while extending template data", "url", extended.GeneratorURL, "err", err.Error())
			return extended
		}

		dashboardUrl, err := url.Parse(extended.DashboardURL)
		if err != nil {
			logger.Debug("failed to parse dashboard URL while extending template data", "url", extended.DashboardURL, "err", err.Error())
			return extended
		}

		orgId := alert.Annotations[ngmodels.OrgIDAnnotation]
		if len(orgId) > 0 {
			extended.DashboardURL = setOrgIdQueryParam(dashboardUrl, orgId)
			extended.PanelURL = setOrgIdQueryParam(u, orgId)
			extended.GeneratorURL = setOrgIdQueryParam(generatorUrl, orgId)
		}
	}

	if alert.Annotations != nil {
		if s, ok := alert.Annotations[ngmodels.ValuesAnnotation]; ok {
			if err := json.Unmarshal([]byte(s), &extended.Values); err != nil {
				logger.Warn("failed to unmarshal values annotation", "error", err)
			}
		}
		// TODO: Remove in Grafana 10
		extended.ValueString = alert.Annotations[ngmodels.ValueStringAnnotation]
	}

	matchers := make([]string, 0)
	for key, value := range alert.Labels {
		if !(strings.HasPrefix(key, "__") && strings.HasSuffix(key, "__")) {
			matchers = append(matchers, key+"="+value)
		}
	}
	sort.Strings(matchers)
	u.Path = path.Join(externalPath, "/alerting/silence/new")

	query := make(url.Values)
	query.Add("alertmanager", "grafana")
	for _, matcher := range matchers {
		query.Add("matcher", matcher)
	}

	u.RawQuery = query.Encode()

	extended.SilenceURL = u.String()

	return extended
}

func setOrgIdQueryParam(url *url.URL, orgId string) string {
	q := url.Query()
	q.Set("orgId", orgId)
	url.RawQuery = q.Encode()

	return url.String()
}

func ExtendData(data *template.Data, logger Logger) *ExtendedData {
	alerts := []ExtendedAlert{}

	for _, alert := range data.Alerts {
		extendedAlert := extendAlert(alert, data.ExternalURL, logger)
		alerts = append(alerts, *extendedAlert)
	}

	extended := &ExtendedData{
		Receiver:          data.Receiver,
		Status:            data.Status,
		Alerts:            alerts,
		GroupLabels:       data.GroupLabels,
		CommonLabels:      removePrivateItems(data.CommonLabels),
		CommonAnnotations: removePrivateItems(data.CommonAnnotations),

		ExternalURL: data.ExternalURL,
	}
	return extended
}

func TmplText(ctx context.Context, tmpl *template.Template, alerts []*types.Alert, l Logger, tmplErr *error) (func(string) string, *ExtendedData) {
	promTmplData := notify.GetTemplateData(ctx, tmpl, alerts, l)
	data := ExtendData(promTmplData, l)

	return func(name string) (s string) {
		if *tmplErr != nil {
			return
		}
		s, *tmplErr = tmpl.ExecuteTextString(name, data)
		return s
	}, data
}

// Firing returns the subset of alerts that are firing.
func (as ExtendedAlerts) Firing() []ExtendedAlert {
	res := []ExtendedAlert{}
	for _, a := range as {
		if a.Status == string(model.AlertFiring) {
			res = append(res, a)
		}
	}
	return res
}

// Resolved returns the subset of alerts that are resolved.
func (as ExtendedAlerts) Resolved() []ExtendedAlert {
	res := []ExtendedAlert{}
	for _, a := range as {
		if a.Status == string(model.AlertResolved) {
			res = append(res, a)
		}
	}
	return res
}
