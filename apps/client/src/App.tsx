import { useState, useRef, useCallback, useEffect } from 'react';
import { User, Group, Message, SelectedChat, DeliveryState, dmKey, groupKey } from './types';
import { createGroup, addMember, listMembers } from './api';
import Sidebar from './components/Sidebar';
import ChatWindow from './components/ChatWindow';
import SetupScreen from './components/SetupScreen';

interface WSEnvelope {
  type: string;
  payload: Record<string, unknown>;
}

function App() {
  const [users, setUsers] = useState<User[]>(() => {
    try { return JSON.parse(localStorage.getItem('wp_users') || '[]'); } catch { return []; }
  });
  const [groups, setGroups] = useState<Group[]>(() => {
    try { return JSON.parse(localStorage.getItem('wp_groups') || '[]'); } catch { return []; }
  });
  const [currentUserId, setCurrentUserId] = useState<string | null>(() =>
    localStorage.getItem('wp_current_user')
  );
  const [connectedUsers, setConnectedUsers] = useState<Set<string>>(new Set());
  const [selectedChat, setSelectedChat] = useState<SelectedChat | null>(null);
  const [messages, setMessages] = useState<Record<string, Message[]>>({});
  const [activeTab, setActiveTab] = useState<'all' | 'personal' | 'groups'>('all');

  const wsMap = useRef<Map<string, WebSocket>>(new Map());

  // Persist users/groups to localStorage
  useEffect(() => {
    localStorage.setItem('wp_users', JSON.stringify(users));
  }, [users]);
  useEffect(() => {
    localStorage.setItem('wp_groups', JSON.stringify(groups));
  }, [groups]);
  useEffect(() => {
    if (currentUserId) localStorage.setItem('wp_current_user', currentUserId);
  }, [currentUserId]);

  // Always-fresh message handler via ref
  const handleMsgRef = useRef<(userId: string, env: WSEnvelope) => void>(null!);
  handleMsgRef.current = (userId: string, env: WSEnvelope) => {
    if (!env.type) return;

    if (env.type === 'message') {
      const { message_id, sender_id, content, group_id } = env.payload as {
        message_id: string;
        sender_id: string;
        content: string;
        group_id?: string;
      };

      const key = group_id
        ? groupKey(group_id)
        : dmKey(sender_id, userId);

      const msg: Message = {
        localId: message_id,
        id: message_id,
        clientId: message_id,
        senderId: sender_id,
        content,
        groupId: group_id,
        state: 1,
        timestamp: new Date(),
      };

      setMessages(prev => ({
        ...prev,
        [key]: [...(prev[key] ?? []).filter(m => m.id !== message_id), msg],
      }));

      // Auto ACK then auto read (simulates real client)
      const ws = wsMap.current.get(userId);
      if (ws?.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'ack', payload: { message_id } }));
        setTimeout(() => {
          if (ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'read', payload: { message_ids: [message_id] } }));
          }
        }, 800);
      }
    }

    if (env.type === 'state_update') {
      const { message_id, state, client_id } = env.payload as {
        message_id: string;
        state: number;
        client_id?: string;
      };

      setMessages(prev => {
        const next: Record<string, Message[]> = {};
        for (const key in prev) {
          next[key] = prev[key].map(m => {
            if ((client_id && m.clientId === client_id) || (m.id && m.id === message_id)) {
              return { ...m, id: message_id, state: state as DeliveryState };
            }
            return m;
          });
        }
        return next;
      });
    }
  };

  const connectUser = useCallback((userId: string) => {
    if (wsMap.current.has(userId)) return;
    const ws = new WebSocket(`/ws?userId=${userId}`);

    ws.onopen = () => {
      setConnectedUsers(prev => new Set([...prev, userId]));
    };
    ws.onclose = () => {
      wsMap.current.delete(userId);
      setConnectedUsers(prev => { const s = new Set(prev); s.delete(userId); return s; });
    };
    ws.onerror = () => ws.close();
    ws.onmessage = e => {
      for (const line of (e.data as string).split('\n')) {
        if (!line.trim()) continue;
        try { handleMsgRef.current(userId, JSON.parse(line)); } catch { /* skip malformed */ }
      }
    };

    wsMap.current.set(userId, ws);
  }, []);

  const disconnectUser = useCallback((userId: string) => {
    wsMap.current.get(userId)?.close();
    wsMap.current.delete(userId);
    setConnectedUsers(prev => { const s = new Set(prev); s.delete(userId); return s; });
  }, []);

  const sendMessage = useCallback((content: string) => {
    if (!currentUserId || !selectedChat || !content.trim()) return;
    const ws = wsMap.current.get(currentUserId);
    if (!ws || ws.readyState !== WebSocket.OPEN) return;

    const clientId = crypto.randomUUID();
    const key = selectedChat.type === 'dm'
      ? dmKey(currentUserId, selectedChat.id)
      : groupKey(selectedChat.id);

    const optimistic: Message = {
      localId: clientId,
      clientId,
      senderId: currentUserId,
      recipientId: selectedChat.type === 'dm' ? selectedChat.id : undefined,
      groupId: selectedChat.type === 'group' ? selectedChat.id : undefined,
      content: content.trim(),
      state: 0,
      timestamp: new Date(),
    };

    setMessages(prev => ({ ...prev, [key]: [...(prev[key] ?? []), optimistic] }));

    const payload: Record<string, string> = { client_id: clientId, content: content.trim() };
    if (selectedChat.type === 'dm') payload.recipient_id = selectedChat.id;
    else payload.group_id = selectedChat.id;

    ws.send(JSON.stringify({ type: 'message', payload }));
  }, [currentUserId, selectedChat]);

  const handleSignIn = (user: User) => {
    setUsers(prev => {
      if (prev.find(u => u.id === user.id)) return prev;
      return [...prev, user];
    });
    setCurrentUserId(user.id);
  };

  const handleCreateGroup = async (name: string, memberIds: string[]) => {
    const group = await createGroup(name);
    for (const uid of memberIds) await addMember(group.id, uid);
    const members = await listMembers(group.id);
    const g: Group = { ...group, memberIds: members.map(m => m.user_id) };
    setGroups(prev => [...prev, g]);
    return g;
  };

  const currentUser = users.find(u => u.id === currentUserId);

  if (!currentUserId || !currentUser) {
    return <SetupScreen onSignIn={handleSignIn} />;
  }

  return (
    <div className="flex h-screen bg-gray-100 overflow-hidden">
      <Sidebar
        currentUser={currentUser}
        users={users}
        groups={groups}
        connectedUsers={connectedUsers}
        selectedChat={selectedChat}
        messages={messages}
        activeTab={activeTab}
        onTabChange={setActiveTab}
        onSelectChat={setSelectedChat}
        onConnect={connectUser}
        onDisconnect={disconnectUser}
        onAddUser={user => setUsers(prev => prev.find(u => u.id === user.id) ? prev : [...prev, user])}
        onCreateGroup={handleCreateGroup}
        onSignOut={() => setCurrentUserId(null)}
      />
      <ChatWindow
        currentUser={currentUser}
        selectedChat={selectedChat}
        messages={selectedChat
          ? (messages[
              selectedChat.type === 'dm'
                ? dmKey(currentUserId, selectedChat.id)
                : groupKey(selectedChat.id)
            ] ?? [])
          : []}
        isRecipientOnline={
          selectedChat?.type === 'dm' ? connectedUsers.has(selectedChat.id) : false
        }
        onSendMessage={sendMessage}
        currentUserId={currentUserId}
      />
    </div>
  );
}

export default App;
