import { Mutex } from '@livekit/mutex';

type QueueTask<T> = () => PromiseLike<T>;

enum QueueTaskStatus {
  'WAITING',
  'RUNNING',
  'COMPLETED',
}

type QueueTaskInfo = {
  id: number;
  enqueuedAt: number;
  executedAt?: number;
  status: QueueTaskStatus;
};

export class AsyncQueue {
  private pendingTasks: Map<number, QueueTaskInfo>;

  private taskMutex: Mutex;

  private nextTaskIndex: number;

  constructor() {
    this.pendingTasks = new Map();
    this.taskMutex = new Mutex();
    this.nextTaskIndex = 0;
  }

  async run<T>(task: QueueTask<T>) {
    const taskInfo: QueueTaskInfo = {
      id: this.nextTaskIndex++,
      enqueuedAt: Date.now(),
      status: QueueTaskStatus.WAITING,
    };
    this.pendingTasks.set(taskInfo.id, taskInfo);
    const unlock = await this.taskMutex.lock();
    try {
      taskInfo.executedAt = Date.now();
      taskInfo.status = QueueTaskStatus.RUNNING;
      return await task();
    } finally {
      taskInfo.status = QueueTaskStatus.COMPLETED;
      this.pendingTasks.delete(taskInfo.id);
      unlock();
    }
  }

  async flush() {
    return this.run(async () => {});
  }

  snapshot() {
    return Array.from(this.pendingTasks.values());
  }
}
