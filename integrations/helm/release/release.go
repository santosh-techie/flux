package release

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/go-kit/kit/log"
	k8shelm "k8s.io/helm/pkg/helm"
	hapi_release "k8s.io/helm/pkg/proto/hapi/release"

	"github.com/weaveworks/flux"
	ifv1 "github.com/weaveworks/flux/apis/helm.integrations.flux.weave.works/v1alpha2"
	fluxk8s "github.com/weaveworks/flux/cluster/kubernetes"
)

var (
	ErrChartGitPathMissing = "Chart deploy configuration (%s) has empty Chart git path"
)

type Action string

const (
	InstallAction Action = "CREATE"
	UpgradeAction Action = "UPDATE"
)

type Config struct {
	ChartsPath string
}

// Release contains clients needed to provide functionality related to helm releases
type Release struct {
	logger log.Logger

	HelmClient *k8shelm.Client

	config Config
}

type Releaser interface {
	GetCurrent() (map[string][]DeployInfo, error)
	GetDeployedRelease(name string) (*hapi_release.Release, error)
	Install(dir string, releaseName string, fhr ifv1.FluxHelmRelease, action Action, opts InstallOptions) (*hapi_release.Release, error)
}

type DeployInfo struct {
	Name string
}

type InstallOptions struct {
	DryRun    bool
	ReuseName bool
}

// New creates a new Release instance.
func New(logger log.Logger, helmClient *k8shelm.Client, config Config) *Release {
	// TODO(michael): check we don't have nil values in the config
	r := &Release{
		logger:     logger,
		HelmClient: helmClient,
		config:     config,
	}
	return r
}

// GetReleaseName either retrieves the release name from the Custom Resource or constructs a new one
//  in the form : $Namespace-$CustomResourceName
func GetReleaseName(fhr ifv1.FluxHelmRelease) string {
	namespace := fhr.Namespace
	if namespace == "" {
		namespace = "default"
	}
	releaseName := fhr.Spec.ReleaseName
	if releaseName == "" {
		releaseName = fmt.Sprintf("%s-%s", namespace, fhr.Name)
	}

	return releaseName
}

// GetDeployedRelease returns a release with Deployed status
func (r *Release) GetDeployedRelease(name string) (*hapi_release.Release, error) {
	rls, err := r.HelmClient.ReleaseContent(name)
	if err != nil {
		return nil, err
	}
	if rls.Release.Info.Status.GetCode() == hapi_release.Status_DEPLOYED {
		return rls.GetRelease(), nil
	}
	return nil, nil
}

func (r *Release) canDelete(name string) (bool, error) {
	rls, err := r.HelmClient.ReleaseStatus(name)

	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Error finding status for release (%s): %#v", name, err))
		return false, err
	}
	/*
		"UNKNOWN":          0,
		"DEPLOYED":         1,
		"DELETED":          2,
		"SUPERSEDED":       3,
		"FAILED":           4,
		"DELETING":         5,
		"PENDING_INSTALL":  6,
		"PENDING_UPGRADE":  7,
		"PENDING_ROLLBACK": 8,
	*/
	status := rls.GetInfo().GetStatus()
	r.logger.Log("info", fmt.Sprintf("Release [%s] status: %s", name, status.Code.String()))
	switch status.Code {
	case 1, 4:
		r.logger.Log("info", fmt.Sprintf("Deleting release (%s)", name))
		return true, nil
	case 2:
		r.logger.Log("info", fmt.Sprintf("Release (%s) already deleted", name))
		return false, nil
	default:
		r.logger.Log("info", fmt.Sprintf("Release (%s) with status %s cannot be deleted", name, status.Code.String()))
		return false, fmt.Errorf("Release (%s) with status %s cannot be deleted", name, status.Code.String())
	}
}

// Install performs a Chart release given the directory containing the
// charts, and the FluxHelmRelease specifying the release. Depending
// on the release type, this is either a new release, or an upgrade of
// an existing one.
func (r *Release) Install(repoDir, releaseName string, fhr ifv1.FluxHelmRelease, action Action, opts InstallOptions) (*hapi_release.Release, error) {
	r.logger.Log("info", fmt.Sprintf("releaseName= %s, action=%s, install options: %+v", releaseName, action, opts))

	chartPath := fhr.Spec.ChartGitPath
	if chartPath == "" {
		r.logger.Log("error", fmt.Sprintf(ErrChartGitPathMissing, fhr.GetName()))
		return nil, fmt.Errorf(ErrChartGitPathMissing, fhr.GetName())
	}

	namespace := fhr.GetNamespace()
	if namespace == "" {
		namespace = "default"
	}

	chartDir := filepath.Join(repoDir, r.config.ChartsPath, chartPath)

	strVals, err := fhr.Spec.Values.YAML()
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Problem with supplied customizations for Chart release [%s]: %#v", releaseName, err))
		return nil, err
	}
	rawVals := []byte(strVals)

	switch action {
	case InstallAction:
		res, err := r.HelmClient.InstallRelease(
			chartDir,
			namespace,
			k8shelm.ValueOverrides(rawVals),
			k8shelm.ReleaseName(releaseName),
			k8shelm.InstallDryRun(opts.DryRun),
			k8shelm.InstallReuseName(opts.ReuseName),
			/*
				helm.InstallReuseName(i.replace),
				helm.InstallDisableHooks(i.disableHooks),
				helm.InstallTimeout(i.timeout),
				helm.InstallWait(i.wait)
			*/
		)

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart release failed: %s: %#v", releaseName, err))
			return nil, err
		}
		if !opts.DryRun {
			err = r.annotateResources(res.Release, fhr)
		}
		return res.Release, err
	case UpgradeAction:
		res, err := r.HelmClient.UpdateRelease(
			releaseName,
			chartDir,
			k8shelm.UpdateValueOverrides(rawVals),
			k8shelm.UpgradeDryRun(opts.DryRun),
			/*
				helm.UpgradeRecreate(u.recreate),
				helm.UpgradeForce(u.force),
				helm.UpgradeDisableHooks(u.disableHooks),
				helm.UpgradeTimeout(u.timeout),
				helm.ResetValues(u.resetValues),
				helm.ReuseValues(u.reuseValues),
				helm.UpgradeWait(u.wait))
			*/
		)

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart upgrade release failed: %s: %#v", releaseName, err))
			return nil, err
		}
		if !opts.DryRun {
			err = r.annotateResources(res.Release, fhr)
		}
		return res.Release, err
	default:
		err = fmt.Errorf("Valid install options: CREATE, UPDATE. Provided: %s", action)
		r.logger.Log("error", err.Error())
		return nil, err
	}
}

// Delete purges a Chart release
func (r *Release) Delete(name string) error {
	ok, err := r.canDelete(name)
	if !ok {
		if err != nil {
			return err
		}
		return nil
	}

	_, err = r.HelmClient.DeleteRelease(name, k8shelm.DeletePurge(true))
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Release deletion error: %#v", err))
		return err
	}
	r.logger.Log("info", fmt.Sprintf("Release deleted: [%s]", name))
	return nil
}

// GetCurrent provides Chart releases (stored in tiller ConfigMaps)
//		output:
//						map[namespace][release name] = nil
func (r *Release) GetCurrent() (map[string][]DeployInfo, error) {
	response, err := r.HelmClient.ListReleases()
	if err != nil {
		return nil, r.logger.Log("error", err)
	}
	r.logger.Log("info", fmt.Sprintf("Number of Chart releases: %d\n", response.GetCount()))

	relsM := make(map[string][]DeployInfo)
	var depl []DeployInfo

	for _, r := range response.GetReleases() {
		ns := r.Namespace
		depl = relsM[ns]

		depl = append(depl, DeployInfo{Name: r.Name})
		relsM[ns] = depl
	}
	return relsM, nil
}

// annotateResources annotates each of the resources created (or updated)
// by the release so that we can spot them.
func (r *Release) annotateResources(release *hapi_release.Release, fhr ifv1.FluxHelmRelease) error {
	args := []string{"annotate", "--overwrite"}
	args = append(args, "--namespace", release.Namespace)
	args = append(args, "-f", "-")
	args = append(args, fluxk8s.AntecedentAnnotation+"="+fhrResourceID(fhr).String())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = bytes.NewBufferString(release.Manifest)

	output, err := cmd.CombinedOutput()
	if err != nil {
		r.logger.Log("output", string(output), "err", err)
	}
	return err
}

// fhrResourceID constructs a flux.ResourceID for a FluxHelmRelease
// resource.
func fhrResourceID(fhr ifv1.FluxHelmRelease) flux.ResourceID {
	return flux.MakeResourceID(fhr.Namespace, "FluxHelmRelease", fhr.Name)
}
