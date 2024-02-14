package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/log"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/template/html/v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

type Deployment struct {
	Name              string `json:"name"`
	Image             string `json:"image"`
	Track             string `json:"track"`
	Replicas          int32  `json:"replicas"`
	AvailableReplicas int32  `json:"availableReplicas"`
}

type Endpoint struct {
	TargetPod string `json:"targetPod"`
	Ip        string `json:"ip"`
}

type AppStateResponse struct {
	CanaryEnabled bool         `json:"canaryEnabled"`
	Deployments   []Deployment `json:"deployments"`
	Endpoints     []Endpoint   `json:"endpoints"`
}

type CanaryCreateRequest struct {
	Tag      string `json:"tag" xml:"tag" form:"tag"`
	Replicas int32  `json:"replicas" xml:"replicas" form:"replicas"`
}

type jsonPatchOp struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	From  string      `json:"from"`
	Value interface{} `json:"value"`
}

func asCustomDeployment(deployments []appsv1.Deployment) []Deployment {
	customDeployments := []Deployment{}
	for _, v := range deployments {
		deployment := Deployment{
			Name:              v.GetName(),
			Image:             v.Spec.Template.Spec.Containers[0].Image,
			Track:             v.GetLabels()["track"],
			Replicas:          *v.Spec.Replicas,
			AvailableReplicas: v.Status.AvailableReplicas,
		}
		customDeployments = append(customDeployments, deployment)
	}
	return customDeployments
}

func asCustomEndpoint(endpoints []v1.EndpointAddress) []Endpoint {
	customEndpoints := []Endpoint{}
	for _, v := range endpoints {
		endpoint := Endpoint{
			TargetPod: v.TargetRef.Name,
			Ip:        v.IP,
		}
		customEndpoints = append(customEndpoints, endpoint)
	}
	return customEndpoints
}

func GetAppState(name string, ctx context.Context, k8s *kubernetes.Clientset) (*AppStateResponse, error) {
	labelSelector := metav1.AddLabelToSelector(&metav1.LabelSelector{}, "app", name)
	options := metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(labelSelector),
	}
	deployments, err := k8s.AppsV1().Deployments("default").List(ctx, options)
	if err != nil {
		return nil, err
	}

	managedDeployments := []appsv1.Deployment{}
	for _, v := range deployments.Items {
		managed, err := strconv.ParseBool(v.Annotations["devops-tool-htmx"])
		if err != nil {
			continue
		}
		if _, ok := v.Labels["track"]; !ok {
			continue
		}
		if managed {
			managedDeployments = append(managedDeployments, v)
		}
	}

	endpoint, err := k8s.CoreV1().Endpoints("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	svc, err := k8s.CoreV1().Services("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	addresses := []v1.EndpointAddress{}
	if len(endpoint.Subsets) > 0 {
		addresses = endpoint.Subsets[0].Addresses
	}

	canaryEnabled := !(svc.Spec.Selector["track"] == "main")

	return &AppStateResponse{
		CanaryEnabled: canaryEnabled,
		Deployments:   asCustomDeployment(managedDeployments),
		Endpoints:     asCustomEndpoint(addresses),
	}, nil
}

func renderApp(c *fiber.Ctx, name string, appState AppStateResponse) error {
	return c.Render("app", fiber.Map{
		"Name":          name,
		"CanaryEnabled": appState.CanaryEnabled,
		"Endpoints":     appState.Endpoints,
		"Deployments":   appState.Deployments,
	})
}

func main() {
	engine := html.New("./views", ".html")

	app := fiber.New(fiber.Config{
		Views: engine,
	})

	app.Use(logger.New(logger.Config{
		Format: "[${ip}]:${port} ${status} - ${method} ${path} - ${error}\n",
	}))

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		log.Fatal(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	app.Get("/", func(c *fiber.Ctx) error {
		apps := make(map[string]bool)
		deployments, err := clientset.AppsV1().Deployments("default").List(c.Context(), metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, v := range deployments.Items {
			app := v.GetLabels()["app"]
			if app != "" {
				apps[app] = true
			}
		}
		keys := make([]string, 0, len(apps))
		for k := range apps {
			keys = append(keys, k)
		}
		return c.Render("index", fiber.Map{
			"Apps": keys,
		})
	})

	app.Get("/app", func(c *fiber.Ctx) error {
		return c.Redirect(c.Query("name"))
	})

	app.Get("/app/:name", func(c *fiber.Ctx) error {
		name := c.Params("name")
		appState, err := GetAppState(name, c.Context(), clientset)
		if err != nil {
			return err
		}
		htmx, _ := strconv.ParseBool(c.Get("HX-Request", "false"))
		if htmx {
			return renderApp(c, name, *appState)
		}
		return c.JSON(appState)
	})

	app.Post("/app/:name/create_canary", func(c *fiber.Ctx) error {
		req := &CanaryCreateRequest{}
		err := c.BodyParser(req)
		if err != nil {
			return err
		}
		name := c.Params("name")
		deployment, err := clientset.AppsV1().Deployments("default").Get(c.Context(), name, metav1.GetOptions{})
		if err != nil {
			return err

		}
		canary_deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-canary-%s", name, strings.ReplaceAll(namesgenerator.GetRandomName(0), "_", "-")),
				Labels: map[string]string{
					"app": "nginx",
				},
				Annotations: map[string]string{
					"devops-tool-htmx": "true",
				},
			},
			Spec: deployment.Spec,
		}
		canary_deployment.ObjectMeta.Labels["track"] = "canary"
		canary_deployment.Spec.Selector.MatchLabels["track"] = "canary"
		canary_deployment.Spec.Template.ObjectMeta.Labels["track"] = "canary"

		canary_deployment.Spec.Replicas = &req.Replicas

		imageSplit := strings.SplitN(canary_deployment.Spec.Template.Spec.Containers[0].Image, ":", 2)
		canary_deployment.Spec.Template.Spec.Containers[0].Image = fmt.Sprintf("%s:%s", imageSplit[0], req.Tag)

		_, err = clientset.AppsV1().Deployments("default").Create(c.Context(), canary_deployment, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		appState, err := GetAppState(c.Params("name"), c.Context(), clientset)
		if err != nil {
			return err
		}
		htmx, _ := strconv.ParseBool(c.Get("HX-Request", "false"))
		if htmx {
			return renderApp(c, name, *appState)
		}
		return c.JSON(appState)
	})

	app.Get("/app/:name/set_canary", func(c *fiber.Ctx) error {
		enabled := c.QueryBool("enabled")
		name := c.Params("name")
		patch := []jsonPatchOp{
			{
				Op:    "add",
				Path:  "/spec/selector/track",
				Value: "main",
				From:  "",
			},
		}
		if enabled {
			patch[0].Op = "remove"
		}
		payload, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		_, err = clientset.CoreV1().Services("default").Patch(c.Context(), name, types.JSONPatchType, payload, metav1.PatchOptions{})
		if err != nil {
			return err
		}

		time.Sleep(100 * time.Millisecond)

		appState, err := GetAppState(c.Params("name"), c.Context(), clientset)
		if err != nil {
			return err
		}
		htmx, _ := strconv.ParseBool(c.Get("HX-Request", "false"))
		if htmx {
			return renderApp(c, name, *appState)
		}
		return c.JSON(appState)
	})

	app.Listen(":3000")
}
