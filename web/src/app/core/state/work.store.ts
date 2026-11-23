import { Injectable, computed, inject, signal } from '@angular/core';
import { ProjectGroup as ActivityProjectGroup, SessionActivity, WorkItem as ActivityWorkItem } from '../models/activity.models';
import { ActivityStore } from './activity.store';
import { activityFromChatStatus, activityFromTaskStatus, groupWork } from './activity-logic';
import { ControlStore } from './control.store';
import { FleetStore } from './fleet.store';
import { OrchestratorStore } from './orchestrator.store';
import { QuestionsStore } from './questions.store';
import { TakenControlStore } from './taken-control.store';
import { AgentChatStore } from './agent-chat.store';
import { SPINNER_WORDS, SpinnerMachine, SpinnerState, spinnerMachine, spinnerStateOf } from '../../shared/spinner-logic';
import { ProjectGroup, WorkColumn, WorkItem, buildWorkItems, groupByProject } from './work-logic';

/** The spinner inputs for one work item. */
export interface ItemSpinner {
  state: SpinnerState;
  machines: SpinnerMachine[];
  coarse: boolean;
  tps?: number;
  /** The activity word shown next to it. */
  word: string;
}

const REVIEW_KEY = 'konsert.review.v1';

interface ReviewState {
  /** id -> 'approved' | 'rejected' */
  decided: Record<string, string>;
  /** ids the user put in the Expedite lane */
  expedite: Record<string, boolean>;
}

function loadReview(): ReviewState {
  try {
    const raw = JSON.parse(localStorage.getItem(REVIEW_KEY) || '{}') as Partial<ReviewState>;
    return { decided: raw.decided ?? {}, expedite: raw.expedite ?? {} };
  } catch {
    return { decided: {}, expedite: {} };
  }
}

/**
 * Selectors over every store for the mode UI: the work list (tasks and agent chats as
 * WorkItems), live work by project, the board, counts. Views read these instead of the
 * raw stores so richer sources (activity state, machine heat, narration) can be swapped
 * in here.
 */
@Injectable({ providedIn: 'root' })
export class WorkStore {
  private readonly fleet = inject(FleetStore);
  private readonly orch = inject(OrchestratorStore);
  private readonly agentChat = inject(AgentChatStore);
  private readonly questions = inject(QuestionsStore);
  private readonly control = inject(ControlStore);
  private readonly taken = inject(TakenControlStore);
  private readonly activity = inject(ActivityStore);

  // --- live-work view model (activity, narration; feat/activity-data) ---

  /**
   * Every task and agent chat grouped by project with counts (running / queued / needs you /
   * blocked / done), each with its live activity and a never-blank narration line.
   */
  readonly projectGroups = computed<ActivityProjectGroup[]>(() => {
    const byKey = this.activity.byKey();
    const tax = { projects: this.fleet.projects(), repos: this.fleet.repos() };
    return groupWork({
      ...tax,
      tasks: this.fleet.tasks(),
      chats: this.orch.sessions(),
      activityOf: (key) => byKey[key],
    });
  });

  /** Every live-work item by "task:<id>" / "chat:<id>". */
  readonly itemsByKey = computed(() => {
    const out: Record<string, ActivityWorkItem> = {};
    for (const g of this.projectGroups()) for (const it of g.items) out[`${it.kind}:${it.id}`] = it;
    return out;
  });

  /** Fleet-wide counts across all groups. */
  readonly totals = computed(() => {
    const t = { running: 0, queued: 0, needs_you: 0, blocked: 0, done: 0, failed: 0 };
    for (const g of this.projectGroups()) {
      for (const k of Object.keys(t) as (keyof typeof t)[]) t[k] += g.counts[k];
    }
    return t;
  });

  /** A task's activity: the reported one, else a coarse one from its status. */
  taskActivity(taskId: string): SessionActivity | undefined {
    const a = this.activity.forTask(taskId);
    if (a) return a;
    const t = this.fleet.tasksById()[taskId];
    return t ? activityFromTaskStatus(t) : undefined;
  }

  /** A chat's activity: the reported one, else a coarse one from its status pill. */
  chatActivity(chatId: string): SessionActivity | undefined {
    const a = this.activity.forChat(chatId);
    if (a) return a;
    const c = this.orch.sessions().find((s) => s.id === chatId);
    return c ? activityFromChatStatus(c) : undefined;
  }

  // --- mode UI selectors (board, live list, reviews) ---

  /** Review decisions and Expedite flags (kept in this browser until the hub stores them). */
  readonly review = signal<ReviewState>(loadReview());

  readonly items = computed<WorkItem[]>(() => {
    const transcripts = this.agentChat.transcripts();
    // Re-evaluate when Take Control changes.
    this.taken.takenControl();
    const narrated = this.itemsByKey();
    const built = buildWorkItems({
      tasks: this.fleet.tasks(),
      chats: this.orch.sessions(),
      workers: this.fleet.workersById(),
      projects: this.fleet.projects(),
      repos: this.fleet.repos(),
      questions: this.questions.all(),
      holds: this.control.holds(),
      interactive: (id) => this.taken.isInteractive(id),
      reviewed: this.review().decided,
      chatStatus: (id) => transcripts[id]?.status,
      chatNow: (id) => transcripts[id]?.subtitle ?? '',
    });
    // Live work says what it is doing now: the orchestrator's narration or the live activity.
    return built.map((it) => {
      const a = narrated[`${it.kind}:${it.id}`];
      if (!a || !it.live || (a.nowSource !== 'orchestrator' && a.nowSource !== 'activity') || !a.now) return it;
      return { ...it, now: a.now };
    });
  });

  /**
   * The live-work spinner for each item (ring state from activity + control, the machine's
   * heat in the avatar): Work cards, Wall tiles, session headers.
   */
  readonly spinnerById = computed(() => {
    const narrated = this.itemsByKey();
    const heat = this.fleet.heatByWorker();
    const workers = this.fleet.workersById();
    const out: Record<string, ItemSpinner> = {};
    for (const it of this.items()) {
      const w = workers[it.workerId];
      const machines = w ? [spinnerMachine(w.name, heat[w.id])] : [];
      if (it.sessionKind === 'terminal') {
        const you = it.control === 'YOU';
        out[it.id] = { state: you ? 'you' : it.live ? 'working' : 'done', machines, coarse: !you && it.live, word: you ? 'you have control' : it.live ? 'shell open' : 'closed' };
        continue;
      }
      const a = narrated[`${it.kind}:${it.id}`]?.activity ?? (it.kind === 'task' ? this.taskActivity(it.id) : this.chatActivity(it.id));
      const state = spinnerStateOf(a, it.control);
      out[it.id] = {
        state,
        machines,
        coarse: !!a?.coarse && it.observed,
        tps: a?.state === 'speaking' ? a.tokens_per_sec : undefined,
        word: SPINNER_WORDS[state],
      };
    }
    return out;
  });

  readonly byId = computed(() => {
    const m: Record<string, WorkItem> = {};
    for (const it of this.items()) m[it.id] = it;
    return m;
  });

  readonly live = computed(() => this.items().filter((i) => i.live));
  readonly liveByProject = computed<ProjectGroup[]>(() => groupByProject(this.live()));
  readonly reviews = computed(() => this.items().filter((i) => i.column === 'review'));

  readonly counts = computed(() => {
    let working = 0;
    let queued = 0;
    let asking = 0;
    let held = 0;
    for (const it of this.live()) {
      if (it.column === 'queued') queued++;
      else if (it.column === 'needs_you') asking++;
      else working++;
      if (it.control === 'HELD' || it.control === 'SUSPENDED') held++;
    }
    return { working, queued, asking, held, review: this.reviews().length };
  });

  /** Running slots in use vs the online fleet's capacity ("14 / 16"). */
  readonly capacity = computed(() => {
    const online = this.fleet.workers().filter((w) => w.status === 'online' && !w.drained);
    const cap = online.reduce((n, w) => n + (w.max_concurrent || 0), 0);
    return cap;
  });

  bySid(sid: string): WorkItem | undefined {
    return this.items().find((i) => i.sid === sid || i.id === sid);
  }

  /** Board cells: column -> items, with Expedite pulled out as its own lane. */
  readonly board = computed(() => {
    const expedite = this.review().expedite;
    const cols: WorkColumn[] = ['queued', 'running', 'needs_you', 'review', 'done'];
    const empty = () => Object.fromEntries(cols.map((c) => [c, [] as WorkItem[]])) as Record<WorkColumn, WorkItem[]>;
    const exp = empty();
    const lanes = new Map<string, { key: string; name: string; cells: Record<WorkColumn, WorkItem[]> }>();
    for (const it of this.items()) {
      if (!it.column) continue;
      if (expedite[it.id] && it.column !== 'done') {
        exp[it.column].push(it);
        continue;
      }
      let lane = lanes.get(it.projectKey);
      if (!lane) {
        lane = { key: it.projectKey, name: it.projectName, cells: empty() };
        lanes.set(it.projectKey, lane);
      }
      lane.cells[it.column].push(it);
    }
    const sorted = [...lanes.values()].sort((a, b) => a.name.localeCompare(b.name));
    return { columns: cols, expedite: exp, lanes: sorted };
  });

  approve(id: string): void {
    this.decide(id, 'approved');
  }

  reject(id: string): void {
    this.decide(id, 'rejected');
  }

  isExpedited(id: string): boolean {
    return !!this.review().expedite[id];
  }

  toggleExpedite(id: string): void {
    this.review.update((r) => {
      const expedite = { ...r.expedite };
      if (expedite[id]) delete expedite[id];
      else expedite[id] = true;
      return { ...r, expedite };
    });
    this.save();
  }

  private decide(id: string, how: string): void {
    this.review.update((r) => ({ ...r, decided: { ...r.decided, [id]: how } }));
    this.save();
  }

  private save(): void {
    try {
      localStorage.setItem(REVIEW_KEY, JSON.stringify(this.review()));
    } catch {
      // storage unavailable: decisions last for this page only
    }
  }
}
