export interface User {
  id: number;
  email?: string;
  displayName: string;
}

export interface Question {
  text: string;
  choices: string[];
  correct: number;
  timeLimit: number;
}

export interface QuizSummary {
  id: number;
  title: string;
  questionCount: number;
  updatedAt: string;
}

export interface Quiz {
  id?: number;
  title: string;
  questions: Question[];
}

export interface GameQuestion {
  text: string;
  choices: string[];
  timeLimit: number;
  deadline: number;
  serverNow: number;
  correct?: number;
}

export interface ScoreRow {
  id: string;
  nick: string;
  score: number;
}

export interface GameState {
  pin: string;
  status: 'lobby' | 'question' | 'reveal' | 'finished';
  quizTitle: string;
  qIndex: number;
  qCount: number;
  question?: GameQuestion;
  answered: number;
  playerCount: number;
  players: { id: string; nick: string }[];
  reveal?: { correct: number; counts: number[] };
  scoreboard?: ScoreRow[];
  you?: {
    id: string;
    nick: string;
    score: number;
    answered: boolean;
    lastPoints: number;
    lastCorrect: boolean;
  };
}

export interface TraceEntry {
  id: number;
  at: number;
  kind: 'http' | 'ws' | 'page';
  method: string;
  path: string;
  status: number | string;
  trace: string;
  hops: number;
}
