import { useState, useRef, useEffect } from 'react';
import { User, Message, SelectedChat } from '../types';
import MessageBubble from './MessageBubble';

interface Props {
  currentUser: User;
  selectedChat: SelectedChat | null;
  messages: Message[];
  isRecipientOnline: boolean;
  currentUserId: string;
  onSendMessage: (content: string) => void;
}

function initials(name: string) {
  return name.slice(0, 2).toUpperCase();
}

export default function ChatWindow({
  currentUser, selectedChat, messages, isRecipientOnline, currentUserId, onSendMessage,
}: Props) {
  const [input, setInput] = useState('');
  const bottomRef = useRef<HTMLDivElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [messages]);

  const handleSend = () => {
    if (!input.trim()) return;
    onSendMessage(input.trim());
    setInput('');
    if (textareaRef.current) {
      textareaRef.current.style.height = 'auto';
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  const handleInputChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setInput(e.target.value);
    e.target.style.height = 'auto';
    e.target.style.height = Math.min(e.target.scrollHeight, 120) + 'px';
  };

  if (!selectedChat) {
    return (
      <div className="flex-1 chat-bg flex flex-col items-center justify-center">
        <div className="text-center">
          <div className="w-16 h-16 rounded-2xl bg-white shadow-sm flex items-center justify-center mx-auto mb-4">
            <svg className="w-8 h-8 text-indigo-400" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={1.5}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
            </svg>
          </div>
          <p className="text-gray-500 font-medium">Select a conversation</p>
          <p className="text-sm text-gray-400 mt-1">Choose from the sidebar to start chatting</p>
        </div>
      </div>
    );
  }

  const grouped = groupByDate(messages);

  return (
    <div className="flex-1 flex flex-col min-w-0">
      {/* Header */}
      <div className="bg-white border-b border-gray-100 px-5 py-3 flex items-center gap-3 flex-shrink-0 shadow-sm z-10">
        <div className="relative">
          <div className={`w-10 h-10 rounded-full flex items-center justify-center text-xs font-semibold ${
            selectedChat.type === 'group' ? 'bg-purple-100 text-purple-600' : 'bg-indigo-100 text-indigo-600'
          }`}>
            {initials(selectedChat.name)}
          </div>
          {selectedChat.type === 'dm' && (
            <div className={`absolute -bottom-0.5 -right-0.5 w-3 h-3 rounded-full border-2 border-white ${isRecipientOnline ? 'bg-emerald-400' : 'bg-gray-300'}`} />
          )}
        </div>
        <div className="flex-1 min-w-0">
          <h2 className="text-sm font-semibold text-gray-900 truncate">{selectedChat.name}</h2>
          <p className={`text-xs ${isRecipientOnline ? 'text-emerald-500' : 'text-gray-400'}`}>
            {selectedChat.type === 'group'
              ? 'Group'
              : isRecipientOnline ? 'Online' : 'Offline'}
          </p>
        </div>
        <div className="flex items-center gap-1">
          <div className="w-px h-5 bg-gray-200 mx-1" />
          <span className="text-xs text-gray-400">
            You: <span className="font-medium text-gray-600">{currentUser.username}</span>
          </span>
        </div>
      </div>

      {/* Messages */}
      <div className="flex-1 overflow-y-auto chat-bg px-4 py-4 space-y-1">
        {messages.length === 0 ? (
          <div className="flex items-center justify-center h-full">
            <div className="bg-white/70 backdrop-blur-sm rounded-2xl px-5 py-3">
              <p className="text-sm text-gray-500">No messages yet — say hello!</p>
            </div>
          </div>
        ) : (
          grouped.map(({ date, msgs }) => (
            <div key={date}>
              {/* Date separator */}
              <div className="flex items-center justify-center my-4">
                <div className="bg-white/70 backdrop-blur-sm text-xs text-gray-500 px-3 py-1 rounded-full shadow-sm">
                  {date}
                </div>
              </div>
              <div className="space-y-1">
                {msgs.map(msg => (
                  <MessageBubble
                    key={msg.localId}
                    message={msg}
                    isMine={msg.senderId === currentUserId}
                    senderName={selectedChat.type === 'group' && msg.senderId !== currentUserId ? msg.senderId.slice(0, 8) : undefined}
                  />
                ))}
              </div>
            </div>
          ))
        )}
        <div ref={bottomRef} />
      </div>

      {/* Input */}
      <div className="bg-white border-t border-gray-100 px-4 py-3 flex-shrink-0">
        <div className="flex items-end gap-2">
          <div className="flex-1 bg-gray-100 rounded-2xl px-4 py-2.5 flex items-end gap-2">
            <textarea
              ref={textareaRef}
              value={input}
              onChange={handleInputChange}
              onKeyDown={handleKeyDown}
              placeholder="Type a message… (Enter to send)"
              rows={1}
              className="flex-1 bg-transparent text-sm text-gray-900 placeholder-gray-400 resize-none focus:outline-none leading-relaxed max-h-28"
            />
          </div>
          <button
            onClick={handleSend}
            disabled={!input.trim()}
            className="w-10 h-10 rounded-full bg-indigo-600 flex items-center justify-center flex-shrink-0 disabled:opacity-30 disabled:cursor-not-allowed hover:bg-indigo-700 transition-colors shadow-sm shadow-indigo-200"
          >
            <svg className="w-4 h-4 text-white" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2.5}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M6 12L3.269 3.126A59.768 59.768 0 0121.485 12 59.77 59.77 0 013.27 20.876L5.999 12zm0 0h7.5" />
            </svg>
          </button>
        </div>
      </div>
    </div>
  );
}

function groupByDate(messages: Message[]) {
  const groups: Record<string, Message[]> = {};
  for (const msg of messages) {
    const d = new Date(msg.timestamp);
    const today = new Date();
    const yesterday = new Date(today);
    yesterday.setDate(today.getDate() - 1);

    let label: string;
    if (isSameDay(d, today)) label = 'Today';
    else if (isSameDay(d, yesterday)) label = 'Yesterday';
    else label = d.toLocaleDateString([], { month: 'short', day: 'numeric' });

    if (!groups[label]) groups[label] = [];
    groups[label].push(msg);
  }
  return Object.entries(groups).map(([date, msgs]) => ({ date, msgs }));
}

function isSameDay(a: Date, b: Date) {
  return a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate();
}
