package main

import (
	"context"
	"database/sql"
	"log/slog"
)

// mk builds a 20-second question; correct is the index into choices.
func mk(text string, correct int, choices ...string) question {
	return question{Text: text, Choices: choices, Correct: correct, TimeLimit: 20}
}

var demoQuizzes = []struct {
	title     string
	questions []question
}{
	{"Computer Networks 101", []question{
		mk("Which transport-layer protocol provides reliable, ordered, connection-oriented delivery?", 1, "UDP", "TCP", "ICMP", "ARP"),
		mk("Which well-known port does HTTPS use by default?", 2, "80", "22", "443", "53"),
		mk("What is the primary job of DNS?", 1, "Maps MAC addresses to IP addresses", "Maps domain names to IP addresses", "Assigns IP addresses to hosts", "Encrypts web traffic"),
		mk("How many usable host addresses are in an IPv4 /24 subnet?", 3, "128", "255", "256", "254"),
		mk("Which HTTP status code means \"Not Found\"?", 1, "500", "404", "200", "301"),
		mk("Which protocol is connectionless and gives no delivery guarantees?", 2, "HTTP", "TLS", "UDP", "TCP"),
		mk("Which three messages make up a TCP connection setup?", 3, "SYN, ACK, FIN", "RST, SYN, FIN", "HELLO, OK, DONE", "SYN, SYN-ACK, ACK"),
		mk("At which OSI layer does IP operate?", 0, "Layer 3 (Network)", "Layer 2 (Data Link)", "Layer 4 (Transport)", "Layer 7 (Application)"),
	}},
	{"Kubernetes & Containers", []question{
		mk("What is the smallest deployable unit in Kubernetes?", 1, "Container", "Pod", "Deployment", "Node"),
		mk("Which component runs on every node and makes sure containers in pods are running?", 2, "kube-scheduler", "etcd", "kubelet", "kube-proxy"),
		mk("Which component stores the cluster's state and configuration?", 0, "etcd", "kube-apiserver", "CoreDNS", "Flannel"),
		mk("Which object gives a set of pods a stable virtual IP and DNS name?", 3, "ConfigMap", "ReplicaSet", "Ingress", "Service"),
		mk("What does a Deployment do?", 0, "Declaratively manages ReplicaSets and rolling updates of pods", "Stores secrets for pods", "Routes external HTTP traffic to services", "Runs a one-off batch job to completion"),
		mk("What is the effect of a taint on a node?", 2, "Forces all pods onto that node", "Encrypts traffic between nodes", "Repels pods that lack a matching toleration", "Deletes unhealthy pods automatically"),
		mk("What is a ConfigMap used for?", 1, "Storing encrypted passwords and tokens", "Storing non-confidential configuration as key-value pairs", "Running background jobs on a schedule", "Allocating persistent disk storage"),
		mk("Which Linux kernel feature isolates what a process can see, such as PIDs and network interfaces?", 1, "cgroups", "namespaces", "iptables", "SELinux"),
	}},
}

// seedDemo inserts the demo quizzes for owner when that owner has none.
// An advisory lock serializes replicas starting at the same time.
func seedDemo(ctx context.Context, db *sql.DB, owner int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7001)`); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM quiz.quizzes WHERE owner_id = $1`, owner).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for _, d := range demoQuizzes {
		if _, err := insertQuiz(ctx, tx, owner, d.title, d.questions); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("seeded demo quizzes", "owner", owner, "count", len(demoQuizzes))
	return nil
}
