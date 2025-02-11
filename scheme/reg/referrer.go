package reg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/regclient/regclient/internal/httplink"
	"github.com/regclient/regclient/internal/reghttp"
	"github.com/regclient/regclient/scheme"
	"github.com/regclient/regclient/types"
	"github.com/regclient/regclient/types/manifest"
	v1 "github.com/regclient/regclient/types/oci/v1"
	"github.com/regclient/regclient/types/platform"
	"github.com/regclient/regclient/types/ref"
	"github.com/regclient/regclient/types/referrer"
)

const OCISubjectHeader = "OCI-Subject"

// ReferrerList returns a list of referrers to a given reference
func (reg *Reg) ReferrerList(ctx context.Context, r ref.Ref, opts ...scheme.ReferrerOpts) (referrer.ReferrerList, error) {
	config := scheme.ReferrerConfig{}
	for _, opt := range opts {
		opt(&config)
	}
	rl := referrer.ReferrerList{
		Subject: r,
		Tags:    []string{},
	}
	// select a platform from a manifest list
	if config.Platform != "" {
		m, err := reg.ManifestHead(ctx, r)
		if err != nil {
			return rl, err
		}
		if m.IsList() {
			m, err = reg.ManifestGet(ctx, r)
			if err != nil {
				return rl, err
			}
			plat, err := platform.Parse(config.Platform)
			if err != nil {
				return rl, err
			}
			d, err := manifest.GetPlatformDesc(m, &plat)
			if err != nil {
				return rl, err
			}
			r.Digest = d.Digest.String()
		} else {
			r.Digest = m.GetDescriptor().Digest.String()
		}
	}
	// if ref is a tag, run a head request for the digest
	if r.Digest == "" {
		m, err := reg.ManifestHead(ctx, r)
		if err != nil {
			return rl, err
		}
		r.Digest = m.GetDescriptor().Digest.String()
	}

	found := false
	// try cache
	rCache := r.SetDigest(r.Digest)
	rl, err := reg.cacheRL.Get(rCache)
	if err == nil {
		found = true
	}
	// try referrers API
	if !found {
		referrerEnabled, ok := reg.featureGet("referrer", r.Registry, r.Repository)
		if !ok || referrerEnabled {
			// attempt to call the referrer API
			rl, err = reg.referrerListByAPI(ctx, r, config)
			if !ok {
				// save the referrer API state
				reg.featureSet("referrer", r.Registry, r.Repository, err == nil)
			}
			if err == nil {
				if config.MatchOpt.ArtifactType == "" {
					// only cache if successful and artifactType is not filtered
					reg.cacheRL.Set(rCache, rl)
				}
				found = true
			}
		}
	}
	// fall back to tag
	if !found {
		rl, err = reg.referrerListByTag(ctx, r)
		if err == nil {
			reg.cacheRL.Set(rCache, rl)
		}
	}
	if err != nil {
		return rl, err
	}

	// apply client side filters and return result
	rl = scheme.ReferrerFilter(config, rl)
	return rl, nil
}

func (reg *Reg) referrerListByAPI(ctx context.Context, r ref.Ref, config scheme.ReferrerConfig) (referrer.ReferrerList, error) {
	rl := referrer.ReferrerList{
		Subject: r,
		Tags:    []string{},
	}
	var link *url.URL
	var resp reghttp.Resp
	// loop for paging
	for {
		rlAdd, respNext, err := reg.referrerListByAPIPage(ctx, r, config, link)
		if err != nil {
			return rl, err
		}
		if rl.Manifest == nil {
			rl = rlAdd
		} else {
			rl.Descriptors = append(rl.Descriptors, rlAdd.Descriptors...)
		}
		resp = respNext
		if resp.HTTPResponse() == nil {
			return rl, fmt.Errorf("missing http response")
		}
		respHead := resp.HTTPResponse().Header
		links, err := httplink.Parse((respHead.Values("Link")))
		if err != nil {
			return rl, err
		}
		next, err := links.Get("rel", "next")
		if err != nil {
			// no next link
			break
		}
		link = resp.HTTPResponse().Request.URL
		if link == nil {
			return rl, fmt.Errorf("referrers list failed to get URL of previous request")
		}
		link, err = link.Parse(next.URI)
		if err != nil {
			return rl, fmt.Errorf("referrers list failed to parse Link: %w", err)
		}
	}
	return rl, nil
}

func (reg *Reg) referrerListByAPIPage(ctx context.Context, r ref.Ref, config scheme.ReferrerConfig, link *url.URL) (referrer.ReferrerList, reghttp.Resp, error) {
	rl := referrer.ReferrerList{
		Subject: r,
		Tags:    []string{},
	}
	query := url.Values{}
	if config.MatchOpt.ArtifactType != "" {
		query.Set("artifactType", config.MatchOpt.ArtifactType)
	}
	req := &reghttp.Req{
		Host: r.Registry,
		APIs: map[string]reghttp.ReqAPI{
			"": {
				Method:     "GET",
				Repository: r.Repository,
				Path:       "referrers/" + r.Digest,
				Query:      query,
				IgnoreErr:  true,
			},
		},
	}
	// replace the API if a link is provided
	if link != nil {
		req.APIs[""] = reghttp.ReqAPI{
			Method:     "GET",
			DirectURL:  link,
			Repository: r.Repository,
		}
	}
	resp, err := reg.reghttp.Do(ctx, req)
	if err != nil {
		return rl, nil, fmt.Errorf("failed to get referrers %s: %w", r.CommonName(), err)
	}
	defer resp.Close()
	if resp.HTTPResponse().StatusCode != 200 {
		return rl, nil, fmt.Errorf("failed to get referrers %s: %w", r.CommonName(), reghttp.HTTPError(resp.HTTPResponse().StatusCode))
	}

	// read manifest
	rawBody, err := io.ReadAll(resp)
	if err != nil {
		return rl, nil, fmt.Errorf("error reading referrers for %s: %w", r.CommonName(), err)
	}

	m, err := manifest.New(
		manifest.WithRef(r),
		manifest.WithHeader(resp.HTTPResponse().Header),
		manifest.WithRaw(rawBody),
	)
	if err != nil {
		return rl, nil, err
	}
	ociML, ok := m.GetOrig().(v1.Index)
	if !ok {
		return rl, nil, fmt.Errorf("unexpected manifest type for referrers: %s, %w", m.GetDescriptor().MediaType, types.ErrUnsupportedMediaType)
	}
	rl.Manifest = m
	rl.Descriptors = ociML.Manifests
	rl.Annotations = ociML.Annotations

	return rl, resp, nil
}

func (reg *Reg) referrerListByTag(ctx context.Context, r ref.Ref) (referrer.ReferrerList, error) {
	rl := referrer.ReferrerList{
		Subject: r,
		Tags:    []string{},
	}
	rlTag, err := referrer.FallbackTag(r)
	if err != nil {
		return rl, err
	}
	m, err := reg.ManifestGet(ctx, rlTag)
	if err != nil {
		if errors.Is(err, types.ErrNotFound) {
			// empty list, initialize a new manifest
			rl.Manifest, err = manifest.New(manifest.WithOrig(v1.Index{
				Versioned: v1.IndexSchemaVersion,
				MediaType: types.MediaTypeOCI1ManifestList,
			}))
			if err != nil {
				return rl, err
			}
			return rl, nil
		}
		return rl, err
	}
	ociML, ok := m.GetOrig().(v1.Index)
	if !ok {
		return rl, fmt.Errorf("manifest is not an OCI index: %s", rlTag.CommonName())
	}
	// return resulting index
	rl.Manifest = m
	rl.Descriptors = ociML.Manifests
	rl.Annotations = ociML.Annotations
	rl.Tags = append(rl.Tags, rlTag.Tag)
	return rl, nil
}

// referrerDelete deletes a referrer associated with a manifest
func (reg *Reg) referrerDelete(ctx context.Context, r ref.Ref, m manifest.Manifest) error {
	// get subject field
	mSubject, ok := m.(manifest.Subjecter)
	if !ok {
		return fmt.Errorf("manifest does not support the subject field: %w", types.ErrUnsupportedMediaType)
	}
	subject, err := mSubject.GetSubject()
	if err != nil {
		return err
	}
	// validate/set subject descriptor
	if subject == nil || subject.MediaType == "" || subject.Digest == "" || subject.Size <= 0 {
		return fmt.Errorf("refers is not set%.0w", types.ErrNotFound)
	}

	// remove from cache
	rSubject := r.SetDigest(subject.Digest.String())
	reg.cacheRL.Delete(rSubject)

	// if referrer API is available, nothing to do, return
	if reg.referrerPing(ctx, rSubject) {
		return nil
	}

	// fallback to using tag schema for refers
	rl, err := reg.referrerListByTag(ctx, rSubject)
	if err != nil {
		return err
	}
	err = rl.Delete(m)
	if err != nil {
		return err
	}
	// push updated referrer list by tag
	rlTag, err := referrer.FallbackTag(rSubject)
	if err != nil {
		return err
	}
	if rl.IsEmpty() {
		err = reg.TagDelete(ctx, rlTag)
		if err == nil {
			return nil
		}
		// if delete is not supported, fall back to pushing empty list
	}
	return reg.ManifestPut(ctx, rlTag, rl.Manifest)
}

// referrerPut pushes a new referrer associated with a manifest
func (reg *Reg) referrerPut(ctx context.Context, r ref.Ref, m manifest.Manifest) error {
	// get subject field
	mSubject, ok := m.(manifest.Subjecter)
	if !ok {
		return fmt.Errorf("manifest does not support the subject field: %w", types.ErrUnsupportedMediaType)
	}
	subject, err := mSubject.GetSubject()
	if err != nil {
		return err
	}
	// validate/set subject descriptor
	if subject == nil || subject.MediaType == "" || subject.Digest == "" || subject.Size <= 0 {
		return fmt.Errorf("subject is not set%.0w", types.ErrNotFound)
	}

	// lock to avoid internal race conditions between pulling and pushing tag
	reg.muRefTag.Lock()
	defer reg.muRefTag.Unlock()
	// fallback to using tag schema for refers
	rSubject := r.SetDigest(subject.Digest.String())
	rl, err := reg.referrerListByTag(ctx, rSubject)
	if err != nil {
		return err
	}
	err = rl.Add(m)
	if err != nil {
		return err
	}
	// ensure the referrer list does not have a subject itself (avoiding circular locks)
	if ms, ok := rl.Manifest.(manifest.Subjecter); ok {
		mDesc, err := ms.GetSubject()
		if err != nil {
			return err
		}
		if mDesc != nil && mDesc.MediaType != "" && mDesc.Size > 0 {
			return fmt.Errorf("fallback referrers manifest should not have a subject: %s", rSubject.CommonName())
		}
	}
	// push updated referrer list by tag
	rlTag, err := referrer.FallbackTag(rSubject)
	if err != nil {
		return err
	}
	if len(rl.Tags) == 0 {
		rl.Tags = []string{rlTag.Tag}
	}
	err = reg.ManifestPut(ctx, rlTag, rl.Manifest)
	if err == nil {
		reg.cacheRL.Set(rSubject, rl)
	}
	return err
}

// referrerPing verifies the registry supports the referrers API
func (reg *Reg) referrerPing(ctx context.Context, r ref.Ref) bool {
	referrerEnabled, ok := reg.featureGet("referrer", r.Registry, r.Repository)
	if ok {
		return referrerEnabled
	}
	req := &reghttp.Req{
		Host: r.Registry,
		APIs: map[string]reghttp.ReqAPI{
			"": {
				Method:     "GET",
				Repository: r.Repository,
				Path:       "referrers/" + r.Digest,
			},
		},
	}
	resp, err := reg.reghttp.Do(ctx, req)
	if err != nil {
		reg.featureSet("referrer", r.Registry, r.Repository, false)
		return false
	}
	_ = resp.Close()
	result := resp.HTTPResponse().StatusCode == 200
	reg.featureSet("referrer", r.Registry, r.Repository, result)
	return result
}
