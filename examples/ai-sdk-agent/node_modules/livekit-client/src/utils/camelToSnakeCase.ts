export type CamelToSnakeCase<Str extends string> = Str extends `${infer First}${infer Rest}`
  ? `${First extends Capitalize<First> ? '_' : ''}${Lowercase<First>}${CamelToSnakeCase<Rest>}`
  : Str;

type ArrayValuesToSnakeCase<Item> = Array<ValueToSnakeCase<Item>>;

type ObjectKeysToSnakeCase<Obj> = {
  [Key in keyof Obj as CamelToSnakeCase<string & Key>]: NonNullable<ValueToSnakeCase<Obj[Key]>>;
};

export type ValueToSnakeCase<Value> =
  Value extends Array<infer Item>
    ? ArrayValuesToSnakeCase<Item>
    : Value extends object
      ? ObjectKeysToSnakeCase<Value>
      : Value;
