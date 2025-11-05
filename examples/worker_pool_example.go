package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Task represents a job to be done.
type Task struct {
	ID int
}

// worker is a goroutine that processes tasks from the tasks channel.
func worker(ctx context.Context, wg *sync.WaitGroup, tasks <-chan Task, results chan<- string) {
	defer wg.Done()
	for {
		select {
		case task, ok := <-tasks:
			if !ok {
				// Tasks channel is closed.
				return
			}
			// Simulate some work
			fmt.Printf("Worker: starting task %d\n", task.ID)
			time.Sleep(2 * time.Second)
			results <- fmt.Sprintf("Task %d completed", task.ID)
		case <-ctx.Done():
			// Context was cancelled.
			fmt.Printf("Worker: cancelled.\n")
			return
		}
	}
}

func main() {
	// Create a context that can be cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	tasks := make(chan Task, 10)
	results := make(chan string, 10)

	// Start 3 workers.
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go worker(ctx, &wg, tasks, results)
	}

	// Add 5 tasks to the tasks channel.
	for i := 1; i <= 5; i++ {
		tasks <- Task{ID: i}
	}
	close(tasks)

	// Start a goroutine to demonstrate cancellation.
	go func() {
		time.Sleep(3 * time.Second)
		fmt.Println("Main: cancelling context.")
		cancel()
	}()

	// Wait for all workers to finish.
	wg.Wait()
	close(results)

	// Print the results.
	for result := range results {
		fmt.Println(result)
	}

	fmt.Println("Main: finished.")
}
