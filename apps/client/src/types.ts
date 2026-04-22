export type DeliveryState = 0 | 1 | 2 | 3 | 4;
// 0=PENDING 1=SENT 2=DELIVERED 3=READ 4=FAILED

export interface User {
  id: string;
  username: string;
  created_at?: string;
}

export interface Group {
  id: string;
  name: string;
  created_at?: string;
  memberIds?: string[];
}

export interface Message {
  localId: string;
  id?: string;
  clientId: string;
  senderId: string;
  recipientId?: string;
  groupId?: string;
  content: string;
  state: DeliveryState;
  timestamp: Date;
}

export interface SelectedChat {
  type: 'dm' | 'group';
  id: string;
  name: string;
}

export function dmKey(a: string, b: string): string {
  return `dm:${[a, b].sort().join('__')}`;
}

export function groupKey(groupId: string): string {
  return `group:${groupId}`;
}
