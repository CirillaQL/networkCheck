package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/prometheus/common/model"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"

	"database/sql"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

type Config struct {
	DeepflowHost               string   `yaml:"deepflow_host"`
	DeepflowClickhouseUsername string   `yaml:"deepflow_clickhouse_username"`
	DeepflowClickhousePassword string   `yaml:"deepflow_clickhouse_password"`
	DeepflowCheckDays          int      `yaml:"deepflow_check_days"`
	IgnoreNamespaces           []string `yaml:"ignore_namespaces"`
	IgnoreDeployments          []string `yaml:"ignore_deployments"`
	IgnoreAnnotations          string   `yaml:"ignore_annotations"`
	PrometheusHost             string   `yaml:"prometheus_host"`
	SafeScale                  bool     `yaml:"safe_scale"`
	ScaleEnable                bool     `yaml:"scale_enable"`
	UseDeepflow                bool     `yaml:"use_deepflow"`
}

func loadConfig() Config {
	config := Config{}
	yamlFile, err := os.ReadFile("./config.yaml")
	if err != nil {
		panic(err)
	}

	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		panic(err)
	}

	return config
}

var FlowLogConn *sql.DB
var FlowTagConn *sql.DB

func NewFlowTagClient(config Config) {
	conn := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{config.DeepflowHost},
		Auth: clickhouse.Auth{
			Database: "flow_tag",
			Username: config.DeepflowClickhouseUsername,
			Password: config.DeepflowClickhousePassword,
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 120,
		},
		DialTimeout: time.Second * 30,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
		Debug:           false,
		BlockBufferSize: 10,
		ClientInfo: clickhouse.ClientInfo{ // optional, please see Client info section in the README.md
			Products: []struct {
				Name    string
				Version string
			}{
				{Name: "checkNetwork", Version: "0.1"},
			},
		},
	})
	conn.SetMaxIdleConns(5)
	conn.SetMaxOpenConns(10)
	conn.SetConnMaxLifetime(time.Hour)
	FlowTagConn = conn
}

func NewFlowLogClient(config Config) {
	conn := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{config.DeepflowHost},
		Auth: clickhouse.Auth{
			Database: "flow_log",
			Username: config.DeepflowClickhouseUsername,
			Password: config.DeepflowClickhousePassword,
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 300,
		},
		DialTimeout: time.Second * 30,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
		Debug:           false,
		BlockBufferSize: 10,
		ClientInfo: clickhouse.ClientInfo{
			Products: []struct {
				Name    string
				Version string
			}{
				{Name: "checkNetwork", Version: "0.1"},
			},
		},
	})
	conn.SetMaxIdleConns(5)
	conn.SetMaxOpenConns(10)
	conn.SetConnMaxLifetime(time.Hour)
	FlowLogConn = conn
}

func FindPodNetworkMonitor(namespace, pod string) bool {
	client, err := api.NewClient(api.Config{
		Address: "https://qa-prometheus.nsstest.com",
	})
	if err != nil {
		fmt.Printf("Error creating client: %v\n", err)
		return false
	}

	networkPromQL := fmt.Sprintf("rate(container_network_receive_bytes_total{namespace=\"%s\",pod=\"%s\"}[3m])", namespace, pod)
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	v1api := v1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := v1.Range{
		Start: midnight,
		End:   time.Now(),
		Step:  time.Minute,
	}
	result, warnings, err := v1api.QueryRange(ctx, networkPromQL, r, v1.WithTimeout(5*time.Second))
	if err != nil {
		fmt.Printf("Error querying Prometheus: %v\n", err)
		return false
	}
	if len(warnings) > 0 {
		fmt.Printf("Warnings: %v\n", warnings)
		return false
	}

	switch t := result.Type(); t {
	case model.ValMatrix:
		help := result.(model.Matrix)
		if len(help) < 1 {
			fmt.Printf("Namespace: %s Pod: %s failed to check network_receive_bytes. Result doesn't include any data \n", namespace, pod)
			return false
		}
		k := help[0]
		for _, sampleValue := range k.Values {
			if sampleValue.Value > 0.001 {
				return false
			}
		}
		return true
	default:
		fmt.Println("Wrong data format. Can't handle this type of prometheus response.")
		return false
	}
}

func CheckDeploymentAffinity(deployment *appsv1.Deployment) bool {
	if deployment.Spec.Template.Spec.Affinity != nil {
		affinity := deployment.Spec.Template.Spec.Affinity
		if affinity.NodeAffinity != nil {
			if affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
				if len(affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) > 0 {
					nodeSelectorTerms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
					for _, term := range nodeSelectorTerms {
						if len(term.MatchExpressions) > 0 {
							for _, match := range term.MatchExpressions {
								for _, value := range match.Values {
									if value == "load" || value == "staging" {
										return true
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return false
}

func main() {
	startTime := time.Now()
	config := loadConfig()
	fmt.Printf("是否启用了安全缩容: %+v \n", config.SafeScale)

	NewFlowLogClient(config)
	NewFlowTagClient(config)

	k8sconfig, err := rest.InClusterConfig()
	if err != nil {
		fmt.Printf("初始化k8s config失败, Error: %v \n", err)
		return
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(k8sconfig)
	if err != nil {
		fmt.Printf("初始化k8s client失败, Error: %v \n", err)
		return
	}

	nowTime := time.Now()
	deltaDay := 0 - config.DeepflowCheckDays
	wantDayTime := nowTime.AddDate(0, 0, deltaDay)

	todayString := fmt.Sprintf("%s 00:00:00.000", wantDayTime.Format("2006-01-02"))

	deployments, err := clientset.AppsV1().Deployments("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fmt.Printf("获取当前集群Deployment List失败, Error: %v \n", err)
		return
	}

	for _, deployment := range deployments.Items {
		if *deployment.Spec.Replicas < 1 {
			continue
		}

		if strings.HasPrefix(deployment.Namespace, "load-") {
			continue
		}

		if deployment.Annotations[config.IgnoreAnnotations] == "true" {
			continue
		}

		var NamespaceFlag bool = false
		for _, ignorePodNamespace := range config.IgnoreNamespaces {
			if ignorePodNamespace == deployment.Namespace {
				NamespaceFlag = true
			}
		}

		if NamespaceFlag {
			continue
		}

		var NameFlag bool = false
		for _, ignoreDeploymentName := range config.IgnoreDeployments {
			if ignoreDeploymentName == deployment.Name {
				NameFlag = true
			}
		}

		if NameFlag {
			continue
		}

		if CheckDeploymentAffinity(&deployment) {
			continue
		}

		// 获取每个 Deployment 控制的 Pods
		set := deployment.Spec.Selector.MatchLabels
		listOptions := metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(metav1.SetAsLabelSelector(set))}
		pods, err := clientset.CoreV1().Pods(deployment.Namespace).List(context.TODO(), listOptions)
		if err != nil {
			fmt.Printf("尝试从Deployment: %s Namespace: %s 中获取其对应的Pod失败,Error: %v \n", deployment.Name, deployment.Namespace, err)
			continue
		}

		for _, pod := range pods.Items {
			networkCheck := FindPodNetworkMonitor(pod.Namespace, pod.Name)
			if networkCheck {
				fmt.Printf("Deployment Pod: %s Namespace: %s 没有网络流量记录，可以直接缩容 \n", pod.Name, pod.Namespace)
				if config.ScaleEnable {
					ScaleDownDeployment(&deployment, clientset)
				}
				continue
			}
			if !config.UseDeepflow {
				// 是否使用deepflow进行检查？如果不适用，可以直接略过
				continue
			} else {
				selectSQL := "SELECT id from flow_tag.pod_map WHERE name = '" + pod.Name + "';"
				rows, err := FlowTagConn.Query(selectSQL)
				if err != nil {
					fmt.Printf("从Deepflow获取Pod: %s Namespace: %s 对应的id失败, Error: %v \n", pod.Name, pod.Namespace, err)
					continue
				}

				for rows.Next() {
					var id int
					err = rows.Scan(&id)
					if err != nil {
						fmt.Printf("从Deepflow获取Pod: %s Namespace: %s 对应的id失败, Error: %v \n", pod.Name, pod.Namespace, err)
						continue
					}
					findLogRecord := fmt.Sprintf("SELECT EXISTS(SELECT * FROM flow_log.l7_flow_log WHERE (pod_id_0 = %d OR pod_id_1 = %d) AND end_time > '%s' LIMIT 1);", id, id, todayString)
					logRows := FlowLogConn.QueryRow(findLogRecord)
					count := 0
					err := logRows.Scan(&count)
					if err != nil {
						fmt.Printf("从Deepflow获取Pod: %s Namespace: %s 对应的日志记录失败, Error: %v \n", pod.Name, pod.Namespace, err)
						continue
					}
					if count == 0 {
						if !config.SafeScale {
							// 危险缩容，只要deepflow里匹配不到就直接缩容
							fmt.Printf("Deployment Pod: %s Namespace: %s 有流量但在deepflow中查不到记录，危险模式下可以缩容 \n", pod.Name, pod.Namespace)
							if config.ScaleEnable {
								ScaleDownDeployment(&deployment, clientset)
							}
						} else {
							fmt.Printf("Deployment Pod: %s Namespace: %s 存在进出流量，但deepflow查询不到实际流量网络连接情况，安全模式下不缩容 \n", pod.Name, pod.Namespace)
						}
						fmt.Println("-------------------------------------------------------------------------")
					}
				}
			}

		}
	}
	endTime := time.Now()
	delta := endTime.Sub(startTime)
	fmt.Println("执行完毕")
	fmt.Printf("共计耗时: %s ", delta)
}

func ScaleDownDeployment(deployment *appsv1.Deployment, clientset *kubernetes.Clientset) {
	fmt.Printf("尝试缩容Deployment: %s Namespace: %s \n", deployment.Name, deployment.Namespace)
	var zeroReplica int32 = 0
	if *deployment.Spec.Replicas >= 1 {
		deployment.Spec.Replicas = &zeroReplica
	} else {
		return
	}
	scaledDeployment, err := clientset.AppsV1().Deployments(deployment.Namespace).Update(context.Background(), deployment, metav1.UpdateOptions{})
	if err != nil || *scaledDeployment.Spec.Replicas != 0 {
		fmt.Printf("Deployment: %s Namespace: %s 缩容失败, Error: %v \n", deployment.Name, deployment.Namespace, err)
	}

	if *scaledDeployment.Spec.Replicas == 0 {
		fmt.Printf("Deployment: %s Namespace: %s 缩容成功 \n", deployment.Name, deployment.Namespace)
	}
}
